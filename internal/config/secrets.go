package config

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
)

func secret32(name string) ([]byte, error) {
	value, path := os.Getenv(name), os.Getenv(name+"_FILE")
	if path != "" {
		if value != "" {
			return nil, errors.New("configure only one of " + name + " or " + name + "_FILE")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, errors.New("cannot open " + name + "_FILE")
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, 257))
		if err != nil || len(raw) > 256 {
			return nil, errors.New("cannot read " + name + "_FILE as a base64 key")
		}
		defer clear(raw)
		value = strings.TrimSpace(string(raw))
	} else if value == "" {
		return nil, nil
	}
	if len(value) == base64.StdEncoding.EncodedLen(32) {
		decoded, err := base64.StdEncoding.Strict().DecodeString(value)
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
		clear(decoded)
	}
	return nil, errors.New(name + " must be 32 base64-encoded random bytes")
}
