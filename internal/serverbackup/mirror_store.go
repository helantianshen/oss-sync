package serverbackup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const markerFile = ".oss-mirror"

func (m *Manager) openMirror(target Mirror) (mirrorStore, error) {
	namespace := "oss-sync-" + target.ID
	switch target.Protocol {
	case Mount:
		if !filepath.IsAbs(target.Address) {
			return nil, errors.New("mounted directory must be an absolute path")
		}
		base, err := filepath.EvalSymlinks(target.Address)
		if err != nil {
			return nil, err
		}
		data, err := filepath.Abs(m.dataDir)
		if err != nil {
			return nil, err
		}
		if data, err = filepath.EvalSymlinks(data); err != nil {
			return nil, err
		}
		if containsPath(data, base) || containsPath(base, data) {
			return nil, errors.New("mirror directory overlaps server storage")
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			return nil, err
		}
		return &mountedMirror{root: root, dir: namespace, marker: target.ID}, nil
	case WebDAV:
		u, err := url.Parse(target.Address)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("enter an HTTP or HTTPS WebDAV directory URL")
		}
		return &davMirror{
			base: *u, dir: namespace, marker: target.ID, user: target.Username, password: target.Password,
			client: &http.Client{
				Timeout:       60 * time.Second,
				CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
			},
		}, nil
	default:
		return nil, errors.New("unsupported mirror protocol")
	}
}

func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

type mountedMirror struct {
	root   *os.Root
	dir    string
	marker string
}

func (m *mountedMirror) close() { _ = m.root.Close() }

func (m *mountedMirror) local(key string) string {
	return filepath.Join(m.dir, filepath.FromSlash(key))
}

func (m *mountedMirror) initialize(_ context.Context) error {
	if err := m.root.Mkdir(m.dir, 0o750); err != nil {
		return err
	}
	file, err := m.root.OpenFile(m.local(markerFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.WriteString(file, m.marker)
	return errors.Join(err, file.Close())
}

func (m *mountedMirror) check(_ context.Context) error {
	info, err := m.root.Lstat(m.dir)
	if err != nil || !info.IsDir() {
		return errors.New("mounted mirror directory is unavailable")
	}
	scope, err := m.root.OpenRoot(m.dir)
	if err != nil {
		return err
	}
	_ = m.root.Close()
	m.root, m.dir = scope, ""
	file, err := m.root.Open(markerFile)
	if err != nil {
		return errors.New("mounted mirror directory is unavailable")
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, 128))
	if err != nil || string(value) != m.marker {
		return errors.New("mounted mirror identity does not match")
	}
	return nil
}

func tempKey(key string) string {
	return path.Join(path.Dir(key), fmt.Sprintf(".oss-tmp-%x", sha256.Sum256([]byte(key))))
}

func (m *mountedMirror) put(ctx context.Context, key string, source *os.File, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	final, tmp := m.local(key), m.local(tempKey(key))
	if err := m.root.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return err
	}
	_ = m.root.Remove(tmp)
	file, err := m.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer m.root.Remove(tmp)
	err = copyChecked(file, source, "", size)
	if err == nil {
		err = file.Sync()
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return m.root.Rename(tmp, final)
}

func (m *mountedMirror) remove(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = m.root.Remove(m.local(tempKey(key)))
	err := m.root.Remove(m.local(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

type davMirror struct {
	base     url.URL
	dir      string
	marker   string
	user     string
	password string
	client   *http.Client
	dirs     map[string]bool
}

func (d *davMirror) close() {}

func (d *davMirror) address(key string) string {
	u := d.base
	u.Path = path.Join("/", u.Path, d.dir, key)
	u.RawPath = ""
	return u.String()
}

func (d *davMirror) request(ctx context.Context, method, key string, body io.Reader, size int64, destination string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.address(key), body)
	if err != nil {
		return nil, err
	}
	if d.user != "" || d.password != "" {
		req.SetBasicAuth(d.user, d.password)
	}
	if body != nil {
		req.ContentLength = size
	}
	if destination != "" {
		req.Header.Set("Destination", d.address(destination))
		req.Header.Set("Overwrite", "T")
	}
	if method == "PROPFIND" {
		req.Header.Set("Depth", "0")
	}
	return d.client.Do(req)
}

func davStatus(response *http.Response, accepted ...int) error {
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	for _, status := range accepted {
		if status == response.StatusCode {
			return nil
		}
	}
	return fmt.Errorf("WebDAV %s returned HTTP %d", response.Request.Method, response.StatusCode)
}

func (d *davMirror) initialize(ctx context.Context) error {
	res, err := d.request(ctx, "MKCOL", "", nil, 0, "")
	if err != nil {
		return err
	}
	if err := davStatus(res, http.StatusCreated); err != nil {
		return err
	}
	res, err = d.request(ctx, http.MethodPut, markerFile, strings.NewReader(d.marker), int64(len(d.marker)), "")
	if err != nil {
		return err
	}
	return davStatus(res, http.StatusCreated, http.StatusNoContent, http.StatusOK)
}

func (d *davMirror) check(ctx context.Context) error {
	res, err := d.request(ctx, http.MethodGet, markerFile, nil, 0, "")
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return davStatus(res, http.StatusOK)
	}
	defer res.Body.Close()
	value, err := io.ReadAll(io.LimitReader(res.Body, 128))
	if err != nil || string(value) != d.marker {
		return errors.New("WebDAV mirror identity does not match")
	}
	d.dirs = make(map[string]bool)
	return nil
}

func (d *davMirror) ensureDir(ctx context.Context, key string) error {
	if key == "." || d.dirs[key] {
		return nil
	}
	if err := d.ensureDir(ctx, path.Dir(key)); err != nil {
		return err
	}
	res, err := d.request(ctx, "MKCOL", key, nil, 0, "")
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusMethodNotAllowed {
		_ = res.Body.Close()
		res, err = d.request(ctx, "PROPFIND", key, nil, 0, "")
		if err != nil {
			return err
		}
		if err := davStatus(res, http.StatusMultiStatus); err != nil {
			return err
		}
	} else if err := davStatus(res, http.StatusCreated); err != nil {
		return err
	}
	d.dirs[key] = true
	return nil
}

func (d *davMirror) put(ctx context.Context, key string, source *os.File, size int64) error {
	if err := d.ensureDir(ctx, path.Dir(key)); err != nil {
		return err
	}
	tmp := tempKey(key)
	var body io.Reader = source
	if size == 0 {
		body = http.NoBody
	}
	res, err := d.request(ctx, http.MethodPut, tmp, body, size, "")
	if err != nil {
		return err
	}
	if err := davStatus(res, http.StatusOK, http.StatusCreated, http.StatusNoContent); err != nil {
		return err
	}
	res, err = d.request(ctx, "MOVE", tmp, nil, 0, key)
	if err != nil {
		return err
	}
	return davStatus(res, http.StatusCreated, http.StatusNoContent, http.StatusOK)
}

func (d *davMirror) remove(ctx context.Context, key string) error {
	for _, entry := range []string{tempKey(key), key} {
		res, err := d.request(ctx, http.MethodDelete, entry, nil, 0, "")
		if err != nil {
			return err
		}
		if err := davStatus(res, http.StatusOK, http.StatusNoContent, http.StatusNotFound); err != nil {
			return err
		}
	}
	return nil
}
