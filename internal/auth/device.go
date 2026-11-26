package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

func newDevice(key []byte, name string, epoch int64) (string, error) {
	id, err := NewID()
	if err != nil {
		return "", err
	}
	return id + "." + deviceMAC(key, name, id, epoch), nil
}

func deviceID(key []byte, name string, epoch int64, value string) (string, bool) {
	id, mac, ok := strings.Cut(value, ".")
	if !ok || id == "" {
		return "", false
	}
	if !hmac.Equal([]byte(mac), []byte(deviceMAC(key, name, id, epoch))) {
		return "", false
	}
	return id, true
}

func deviceMAC(key []byte, name, id string, epoch int64) string {
	msg := strings.ToLower(name) + "\x00" + id
	if epoch != 0 {
		msg += "\x00" + strconv.FormatInt(epoch, 10)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
