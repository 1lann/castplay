package airplay

import (
	"encoding/hex"
	"errors"
)

// FairPlay v3 setup ("fp-setup"). Senders run this handshake when feature
// bit 14 is advertised, which iOS Music requires. With HomeKit pairing the
// stream key arrives in SETUP ("shk"), so only the two handshake replies are
// needed, never FairPlay decryption: a canned 142-byte reply per mode, then
// a fixed header plus the last 20 bytes of the request. Replies from
// openairplay/airplay2-receiver (ap2/playfair.py), as used by UxPlay/RPiPlay.

var fpReplies = func() [4][]byte {
	var out [4][]byte
	for i, h := range []string{
		"46504c59030102000000008202000f9f3f9e0a2521dbdf312ab2bfb29e8d232b6376a8c818701d22ae93d82737feaf9db4fdf41c2dba9d1f49caaabf6591ac1f7bc6f7e0663d21afe01565953eab81f418ceed095adb7c3d0e254909a79831d49c3982973434facb42c63a1cd911a6fe941a8a6d4a743b46c3a7649e44c78955e49d8155009549c4e2f7a3f6d5ba",
		"46504c5903010200000000820201cf32a25714b2524f8aa0ad7af164e37bcf4424e200047efc0ad67afcd95ded1c2730bb591b962ed63a9c4ded88ba8fc78de64d91ccfd5c7b56da88e31f5cceafc7431995a01665a54e1939d25b94db64b9e45d8d063e1e6af07e9656162b0efa404275ea5a44d9591c7256b9fbe6513898b80227721988571650942ad946688a",
		"46504c5903010200000000820202c169a352eeed35b18cdd9c58d64f16c1519a89eb5317bd0d4336cd68f638ff9d016a5b52b7fa9216b2b65482c78444118121a2c7fed83db7119e9182aad7d18c7063e2a457555910af9e0efc76347d164043807f581ee4fbe42ca9dedc1b5eb2a3aa3d2ecd59e7eee70b3629f22afd161d877353ddb99adc8e07006e56f850ce",
		"46504c59030102000000008202039001e1727e0f57f9f5880db104a6257a23f5cfff1abbe1e93045251afb97eb9fc0011ebe0f3a81df5b691d76acb2f7a5c708e3d328f56bb39dbde5f29c8a17f481487e3ae863c678325422e6f78e166d18aa7fd636258bce28726f661f738893ce44311e4be6c0535193e5ef72e8686233729c227d820c999445d89246c8c359",
	} {
		out[i], _ = hex.DecodeString(h)
	}
	return out
}()

var fpHeader = []byte{0x46, 0x50, 0x4c, 0x59, 0x03, 0x01, 0x04, 0x00, 0x00, 0x00, 0x00, 0x14}

func fairplaySetup(req []byte) ([]byte, error) {
	if len(req) < 16 || string(req[:4]) != "FPLY" || req[4] != 3 {
		return nil, errors.New("fp-setup: unsupported FairPlay message")
	}
	if req[5] != 1 { // message type: setup
		return nil, errors.New("fp-setup: not a setup message")
	}
	switch req[6] {
	case 1:
		mode := req[14]
		if mode > 3 {
			return nil, errors.New("fp-setup: bad mode")
		}
		return fpReplies[mode], nil
	case 3:
		if len(req) < 20 {
			return nil, errors.New("fp-setup: short message")
		}
		return append(append([]byte{}, fpHeader...), req[len(req)-20:]...), nil
	}
	return nil, errors.New("fp-setup: unexpected sequence")
}
