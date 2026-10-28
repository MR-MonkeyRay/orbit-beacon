package main

import (
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"orbitbeacon/internal/server/securefs"
)

func readRestorePassword(path string) (string, error) {
	var password []byte
	if path == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return "", errors.New("restore_password_required: use --password-file for non-interactive restore")
		}
		fmtPrompt := []byte("Archive password: ")
		if _, err := os.Stderr.Write(fmtPrompt); err != nil {
			return "", err
		}
		var err error
		password, err = term.ReadPassword(int(os.Stdin.Fd()))
		os.Stderr.WriteString("\n")
		if err != nil {
			return "", err
		}
	} else {
		file, err := securefs.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			return "", errors.New("restore_password_invalid: password file must be an owned 0600 regular file")
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return "", err
		}
		if info.Mode().Perm() != 0600 || info.Size() < 16 || info.Size() > 17 {
			file.Close()
			return "", errors.New("restore_password_invalid")
		}
		password, err = io.ReadAll(io.LimitReader(file, 18))
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			clear(password)
			return "", errors.Join(err, closeErr)
		}
		if len(password) == 17 && password[16] == '\n' {
			password = password[:16]
		}
	}
	defer clear(password)
	if len(password) != 16 || strings.Trim(string(password), "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return "", errors.New("restore_password_invalid")
	}
	return string(password), nil
}
