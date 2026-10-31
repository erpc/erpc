package streams

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileCursor stores the cursor in the file at path. A save writes a
// temporary file in the same directory, syncs it and renames it over path,
// so a crash leaves the old token or the new one.
func FileCursor(path string) Cursor { return fileCursor(path) }

type fileCursor string

func (p fileCursor) Load() (string, error) {
	b, err := os.ReadFile(string(p))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

func (p fileCursor) Save(token string) error {
	f, err := os.CreateTemp(filepath.Dir(string(p)), filepath.Base(string(p))+".*")
	if err != nil {
		return err
	}
	_, err = f.WriteString(token + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), string(p))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
