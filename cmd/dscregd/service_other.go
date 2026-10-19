//go:build !windows

package main

func platformService([]string) (bool, error) {
	return false, nil
}
