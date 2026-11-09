// Wire layout and limits match internal/syncapi/delta.go.
export const DELTA_CONTENT_TYPE = "application/vnd.oss-sync.delta-v1";
export const DELTA_SIGNATURE_TYPE = "application/vnd.oss-sync.signature-v1";
const FINGERPRINT_BYTES = 20;
const COPY_FLAG = 0x80000000;

export function canUseMarkdownDelta(size: number): boolean {
  return size >= 256 && size <= 32 * 1024 * 1024;
}

function blockSizeFor(size: number): number {
  if (size >= 1 << 20) return 4096;
  if (size >= 64 << 10) return 1024;
  if (size >= 4 << 10) return 256;
  return 64;
}

function rollingChecksum(block: Uint8Array): [number, number] {
  let sum = 0;
  let weighted = 0;
  for (const value of block) {
    sum += value;
    weighted += sum;
  }
  return [sum, weighted];
}

function weakHash(sum: number, weighted: number): number {
  return (sum & 0xffff) | (weighted << 16);
}

async function sha256(bytes: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", bytes.slice()));
}

export async function markdownSignature(base: Uint8Array): Promise<ArrayBuffer> {
  const blockSize = blockSizeFor(base.length);
  const buffer = new ArrayBuffer(4 + Math.floor(base.length / blockSize) * FINGERPRINT_BYTES);
  const data = new DataView(buffer);
  const bytes = new Uint8Array(buffer);
  data.setUint32(0, base.length, true);
  for (let offset = 0, cursor = 4; offset + blockSize <= base.length; offset += blockSize, cursor += FINGERPRINT_BYTES) {
    const block = base.subarray(offset, offset + blockSize);
    data.setInt32(cursor, weakHash(...rollingChecksum(block)), true);
    bytes.set((await sha256(block)).subarray(0, 16), cursor + 4);
  }
  return buffer;
}

interface DeltaCommand {
  readonly copy: boolean;
  readonly offset: number;
  length: number;
}

export async function buildMarkdownDelta(signature: ArrayBuffer, target: Uint8Array): Promise<ArrayBuffer | null> {
  if (signature.byteLength < 4 || !canUseMarkdownDelta(target.length)) return null;
  const fingerprint = new DataView(signature);
  const baseSize = fingerprint.getUint32(0, true);
  const blockSize = blockSizeFor(baseSize);
  if (!canUseMarkdownDelta(baseSize) || target.length < blockSize ||
      signature.byteLength !== 4 + Math.floor(baseSize / blockSize) * FINGERPRINT_BYTES) return null;

  const blocks = new Map<number, Map<string, number>>();
  for (let cursor = 4; cursor < signature.byteLength; cursor += FINGERPRINT_BYTES) {
    const weak = fingerprint.getInt32(cursor, true);
    const strong = String.fromCharCode(...new Uint8Array(signature, cursor + 4, 16));
    let matches = blocks.get(weak);
    if (!matches) blocks.set(weak, matches = new Map());
    matches.set(strong, (cursor - 4) / FINGERPRINT_BYTES * blockSize);
  }

  const commands: DeltaCommand[] = [];
  let budget = Math.floor(target.length / blockSize) * 4 + 16;
  let literalStart = 0;
  let [sum, weighted] = rollingChecksum(target.subarray(0, blockSize));
  for (let position = 0; position + blockSize <= target.length;) {
    const candidates = blocks.get(weakHash(sum, weighted));
    let offset: number | undefined;
    if (candidates) {
      if (budget-- === 0) return null;
      const hash = await sha256(target.subarray(position, position + blockSize));
      offset = candidates.get(String.fromCharCode(...hash.subarray(0, 16)));
    }
    if (offset !== undefined) {
      if (position > literalStart) {
        commands.push({ copy: false, offset: literalStart, length: position - literalStart });
      }
      const previous = commands.at(-1);
      if (previous?.copy && previous.offset + previous.length === offset) previous.length += blockSize;
      else commands.push({ copy: true, offset, length: blockSize });

      position += blockSize;
      literalStart = position;
      [sum, weighted] = rollingChecksum(target.subarray(position, position + blockSize));
    } else {
      if (position + blockSize < target.length) {
        const outgoing = target[position];
        sum = (sum + target[position + blockSize] - outgoing) & 0xffff;
        weighted = (weighted + sum - blockSize * outgoing) & 0xffff;
      }
      position++;
    }
  }
  if (literalStart < target.length) {
    commands.push({ copy: false, offset: literalStart, length: target.length - literalStart });
  }
  const size = commands.reduce((length, command) => length + 4 + (command.copy ? 4 : command.length), 4);
  if (size + signature.byteLength >= target.length) return null;

  const result = new ArrayBuffer(size);
  const bytes = new Uint8Array(result);
  const data = new DataView(result);
  data.setUint32(0, target.length, true);
  let cursor = 4;
  for (const { copy, offset, length } of commands) {
    data.setUint32(cursor, copy ? length | COPY_FLAG : length, true);
    cursor += 4;
    if (copy) {
      data.setUint32(cursor, offset, true);
      cursor += 4;
    } else {
      bytes.set(target.subarray(offset, offset + length), cursor);
      cursor += length;
    }
  }
  return result;
}

export async function applyMarkdownDelta(
  base: Uint8Array,
  patch: ArrayBuffer,
  target: { readonly size: number; readonly hash: string },
): Promise<ArrayBuffer | null> {
  if (patch.byteLength < 4 || patch.byteLength >= target.size || !canUseMarkdownDelta(target.size)) return null;
  const data = new DataView(patch);
  if (data.getUint32(0, true) !== target.size) return null;

  const output = new Uint8Array(target.size);
  let cursor = 4;
  let written = 0;
  while (cursor + 4 <= data.byteLength) {
    const command = data.getUint32(cursor, true);
    cursor += 4;
    const length = command & ~COPY_FLAG;
    if (length === 0 || length > target.size - written) return null;
    if ((command & COPY_FLAG) !== 0) {
      if (cursor + 4 > data.byteLength) return null;
      const offset = data.getUint32(cursor, true);
      if (offset + length > base.length) return null;
      output.set(base.subarray(offset, offset + length), written);
      cursor += 4;
    } else {
      if (cursor + length > data.byteLength) return null;
      output.set(new Uint8Array(patch, cursor, length), written);
      cursor += length;
    }
    written += length;
  }
  if (cursor !== data.byteLength || written !== target.size) return null;
  const hash = Array.from(await sha256(output), (byte) => byte.toString(16).padStart(2, "0")).join("");
  return hash === target.hash ? output.buffer : null;
}
