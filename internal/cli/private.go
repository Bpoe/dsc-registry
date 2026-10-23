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
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		cleanup := os.Remove(path)
		return errors.Join(fmt.Errorf("cannot finish protected file"), cleanup)
	}
	return nil
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
