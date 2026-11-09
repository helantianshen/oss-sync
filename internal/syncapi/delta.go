package syncapi

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"

	"github.com/helantianshen/oss-sync/internal/models"
)

// Both clients use little-endian sizes and byte offsets. Signatures contain
// only fingerprints, so neither peer needs to keep a second copy of a note.
const (
	deltaContentType   = "application/vnd.oss-sync.delta-v1"
	deltaSignatureType = "application/vnd.oss-sync.signature-v1"
	deltaMaxBlockSize  = 4096
	deltaMinBytes      = 256
	deltaMaxBytes      = 32 << 20
	deltaFingerprint   = 20 // rolling checksum + first 16 bytes of SHA-256
	deltaSignatureMax  = 4 + deltaMaxBytes/deltaMaxBlockSize*deltaFingerprint
	deltaCopyFlag      = uint32(1 << 31)
)

var (
	errInvalidDelta = errors.New("invalid Markdown delta")
	errDeltaBase    = errors.New("Markdown delta base is unavailable")
)

func deltaSizeAllowed(size int64) bool {
	return size >= deltaMinBytes && size <= deltaMaxBytes
}

func deltaBlockSize(size int) int {
	switch {
	case size >= 1<<20:
		return deltaMaxBlockSize
	case size >= 64<<10:
		return 1024
	case size >= 4<<10:
		return 256
	default:
		return 64
	}
}

func rollingDeltaChecksum(block []byte) (a, b uint32) {
	for _, value := range block {
		a += uint32(value)
		b += a
	}
	return a, b
}

func deltaWeak(a, b uint32) uint32 {
	return a&math.MaxUint16 | b<<16
}

func markdownSignature(base []byte) []byte {
	blockSize := deltaBlockSize(len(base))
	signature := make([]byte, 4, 4+len(base)/blockSize*deltaFingerprint)
	binary.LittleEndian.PutUint32(signature, uint32(len(base)))
	for offset := 0; offset+blockSize <= len(base); offset += blockSize {
		block := base[offset : offset+blockSize]
		signature = binary.LittleEndian.AppendUint32(signature, deltaWeak(rollingDeltaChecksum(block)))
		hash := sha256.Sum256(block)
		signature = append(signature, hash[:16]...)
	}
	return signature
}

// buildMarkdownDelta finds intact blocks even if an insertion or deletion
// shifted their offsets. A strong hash filters weak-checksum collisions.
func buildMarkdownDelta(signature, target []byte) []byte {
	if len(signature) < 4 || !deltaSizeAllowed(int64(len(target))) {
		return nil
	}
	baseSize := int(binary.LittleEndian.Uint32(signature))
	blockSize := deltaBlockSize(baseSize)
	if !deltaSizeAllowed(int64(baseSize)) || len(target) < blockSize || len(signature) != 4+baseSize/blockSize*deltaFingerprint {
		return nil
	}
	type strongHash [16]byte
	blocks := make(map[uint32]map[strongHash]int)
	for i := 4; i < len(signature); i += deltaFingerprint {
		weak := binary.LittleEndian.Uint32(signature[i:])
		strong := strongHash(signature[i+4 : i+deltaFingerprint])
		if blocks[weak] == nil {
			blocks[weak] = make(map[strongHash]int)
		}
		blocks[weak][strong] = (i - 4) / deltaFingerprint * blockSize
	}
	// Account for the signature sent by the peer, too.
	limit := len(target) - len(signature)
	if limit <= 4 {
		return nil
	}
	patch := binary.LittleEndian.AppendUint32(nil, uint32(len(target)))
	budget := len(target)/blockSize*4 + 16
	a, b := rollingDeltaChecksum(target[:blockSize])
	lastCopy, literalStart := -1, 0
	for position := 0; position+blockSize <= len(target); {
		block := target[position : position+blockSize]
		offset, matched := 0, false
		if candidates := blocks[deltaWeak(a, b)]; candidates != nil {
			if budget == 0 {
				return nil
			}
			budget--
			hash := sha256.Sum256(block)
			offset, matched = candidates[strongHash(hash[:16])]
		}
		if matched {
			if position > literalStart {
				if len(patch)+4+position-literalStart >= limit {
					return nil
				}
				patch = binary.LittleEndian.AppendUint32(patch, uint32(position-literalStart))
				patch = append(patch, target[literalStart:position]...)
				lastCopy = -1
			}
			if lastCopy >= 0 && int(binary.LittleEndian.Uint32(patch[lastCopy+4:]))+
				int(binary.LittleEndian.Uint32(patch[lastCopy:])&^deltaCopyFlag) == offset {
				length := binary.LittleEndian.Uint32(patch[lastCopy:])
				binary.LittleEndian.PutUint32(patch[lastCopy:], length+uint32(blockSize))
			} else {
				lastCopy = len(patch)
				patch = binary.LittleEndian.AppendUint32(patch, deltaCopyFlag|uint32(blockSize))
				patch = binary.LittleEndian.AppendUint32(patch, uint32(offset))
			}
			position += blockSize
			literalStart = position
			if position+blockSize <= len(target) {
				a, b = rollingDeltaChecksum(target[position : position+blockSize])
			}
		} else {
			if position+blockSize < len(target) {
				out, in := uint32(target[position]), uint32(target[position+blockSize])
				a += in - out
				b += a - uint32(blockSize)*out
			}
			position++
		}
	}
	if literalStart < len(target) {
		if len(patch)+4+len(target)-literalStart >= limit {
			return nil
		}
		patch = binary.LittleEndian.AppendUint32(patch, uint32(len(target)-literalStart))
		patch = append(patch, target[literalStart:]...)
	}
	if len(patch) >= limit {
		return nil
	}
	return patch
}

func markdownDeltaSize(patch []byte) (int64, error) {
	if len(patch) < 4 {
		return 0, errInvalidDelta
	}
	size := int64(binary.LittleEndian.Uint32(patch))
	if !deltaSizeAllowed(size) {
		return 0, errInvalidDelta
	}
	return size, nil
}

func applyMarkdownDelta(base, patch []byte) ([]byte, error) {
	size, err := markdownDeltaSize(patch)
	if err != nil {
		return nil, err
	}
	output := make([]byte, 0, size)
	for patch = patch[4:]; len(patch) >= 4; {
		command := binary.LittleEndian.Uint32(patch)
		patch = patch[4:]
		length := int(command &^ deltaCopyFlag)
		if length == 0 || length > cap(output)-len(output) {
			return nil, errInvalidDelta
		}
		if command&deltaCopyFlag != 0 {
			if len(patch) < 4 || length > len(base) {
				return nil, errInvalidDelta
			}
			offset := uint64(binary.LittleEndian.Uint32(patch))
			if offset > uint64(len(base)-length) {
				return nil, errInvalidDelta
			}
			output = append(output, base[offset:offset+uint64(length)]...)
			patch = patch[4:]
		} else {
			if length > len(patch) {
				return nil, errInvalidDelta
			}
			output = append(output, patch[:length]...)
			patch = patch[length:]
		}
	}
	if len(patch) != 0 || int64(len(output)) != size {
		return nil, errInvalidDelta
	}
	return output, nil
}

func readDeltaBytes(reader io.Reader, limit int64) ([]byte, error) {
	bytes, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(bytes)) > limit {
		return nil, errInvalidDelta
	}
	return bytes, nil
}

func writePatchedUpload(basePath, destination, baseHash, targetHash string, patch []byte) error {
	file, err := os.Open(basePath)
	if err != nil {
		return errDeltaBase
	}
	defer file.Close()
	base, err := readDeltaBytes(file, deltaMaxBytes)
	if err != nil || !matchesDeltaHash(base, baseHash) {
		return errDeltaBase
	}
	output, err := applyMarkdownDelta(base, patch)
	if err != nil || !matchesDeltaHash(output, targetHash) {
		return errInvalidDelta
	}
	return os.WriteFile(destination, output, 0o600)
}

func matchesDeltaHash(content []byte, expected string) bool {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:]) == expected
}

func serveMarkdownDelta(c *gin.Context, file io.Reader, size int64, meta models.File, representation string) {
	c.Header("Cache-Control", "no-store")
	if meta.Type != "markdown" || !deltaSizeAllowed(size) || size != meta.Size {
		c.Status(http.StatusNoContent)
		return
	}
	var signature []byte
	if representation == deltaContentType {
		if c.GetHeader("Content-Type") != deltaSignatureType {
			c.Status(http.StatusUnsupportedMediaType)
			return
		}
		var err error
		signature, err = readDeltaBytes(c.Request.Body, deltaSignatureMax)
		if err != nil {
			c.Status(http.StatusNoContent)
			return
		}
	}
	content, err := readDeltaBytes(file, deltaMaxBytes)
	if err != nil || !matchesDeltaHash(content, meta.Hash) {
		c.Status(http.StatusNoContent)
		return
	}
	if representation == deltaSignatureType {
		c.Data(http.StatusOK, deltaSignatureType, markdownSignature(content))
	} else if patch := buildMarkdownDelta(signature, content); patch != nil {
		c.Data(http.StatusOK, deltaContentType, patch)
	} else {
		c.Status(http.StatusNoContent)
	}
}
