package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/Bpoe/dsc-registry/internal/clidoc"
)

func validateServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return invalid("server must be an http(s) URL without credentials, query, or fragment")
	}
	return nil
}

func (a *app) printDocument(value any, format string) error {
	if format != "json" && format != "yaml" {
		return invalid("this command requires json or yaml output")
	}
	data, err := clidoc.Encode(value, format)
	if err != nil {
		return err
	}
	_, err = a.out.Write(data)
	return err
}

func writePrivateNew(path string, data []byte) error {
	f, err := createPrivate(path)
	if err != nil {
		return fmt.Errorf("cannot create protected file (destination must not already exist)")
	}
	if err := finishPrivateFile(f, data); err != nil {
		cleanup := os.Remove(path)
		return errors.Join(fmt.Errorf("cannot finish protected file"), cleanup)
	}
	return nil
}

type privateFile interface {
	io.Writer
	Sync() error
	Close() error
}

func finishPrivateFile(f privateFile, data []byte) error {
	n, writeErr := f.Write(data)
	if n != len(data) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func (a *app) issueCredential(resource, field, destination string, issue func() (map[string]any, error)) (err error) {
	var file *os.File
	closed, saved := false, false
	if destination != "" {
		file, err = createPrivate(destination)
		if err != nil {
			return fmt.Errorf("cannot create protected file (destination must not already exist)")
		}
		defer func() {
			if !closed {
				if closeErr := file.Close(); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("cannot close reserved credential file"))
				}
			}
			if !saved {
				if removeErr := os.Remove(destination); removeErr != nil {
					err = errors.Join(err, fmt.Errorf("cannot remove incomplete credential file"))
				}
			}
		}()
	}
	value, err := issue()
	if err != nil {
		return err
	}
	if file != nil {
		err = saveIssuedCredential(file, value, resource, field)
		closed = true
		if err != nil {
			return err
		}
		saved = true
		delete(value, field)
	}
	return a.secretResult(value, resource, field, destination)
}

func saveIssuedCredential(file privateFile, value map[string]any, resource, field string) error {
	secret, ok := value[field].(string)
	var err error
	if !ok || secret == "" {
		err = errors.Join(fmt.Errorf("server did not return the issued credential"), file.Close())
	} else {
		err = finishPrivateFile(file, []byte(secret+"\n"))
	}
	if err == nil {
		return nil
	}
	if resource == "api-key" {
		return fmt.Errorf("API key %q was issued but could not be saved; revoke this key before retrying", display(value["keyId"]))
	}
	return fmt.Errorf("registration token was issued but could not be saved; it cannot be revoked and remains valid until %s", display(value["expiresAt"]))
}

func readPrivate(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot open API key file")
	}
	defer f.Close()
	if err := checkPrivate(f); err != nil {
		return "", err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", invalid("API key file must be a regular protected file")
	}
	link, err := os.Lstat(path)
	if err != nil || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, link) {
		return "", invalid("API key file must not be a symbolic link")
	}
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil {
		return "", fmt.Errorf("cannot read API key file")
	}
	if len(data) > 8192 {
		return "", invalid("API key file exceeds 8192 bytes")
	}
	return string(data), nil
}
