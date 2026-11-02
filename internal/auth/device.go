package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

// A device is a browser that has logged in as a name before: it holds a
// value made for that name, <id>.<mac>, where mac is an HMAC-SHA256 of the
// name, the id and the user's device epoch under the server's device key.
// A new password starts a new epoch, so a browser that logged in with the
// old one, somebody else's perhaps, is a device no more. Its attempts are counted
// on a counter of their own (OWASP's device cookies), so that whoever
// sends wrong passwords for the name from elsewhere throttles only those
// who have never logged in as it, and never its owner's own browser.

// newDevice makes a device value for name at epoch.
func newDevice(key []byte, name string, epoch int64) (string, error) {
	id, err := NewID()
	if err != nil {
		return "", err
	}
	return id + "." + deviceMAC(key, name, id, epoch), nil
}

// deviceID is the id of value when value was made for name at epoch under
// key. A value made for another name or another epoch, or by anybody
// without the key, is no device at all. The comparison takes the same time however much of the
// MAC is right.
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

// deviceMAC is over the name in lower case, as the throttle counts it, the
// id and, past epoch 0, the epoch, with a NUL between each: neither the
// name nor the id holds one. Epoch 0 signs as values were signed before
// there were epochs, so those still count until the next new password.
func deviceMAC(key []byte, name, id string, epoch int64) string {
	msg := strings.ToLower(name) + "\x00" + id
	if epoch != 0 {
		msg += "\x00" + strconv.FormatInt(epoch, 10)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
