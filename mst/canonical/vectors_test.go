package canonical

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate expected values in testvectors/vectors.json")

const vectorsPath = "../testvectors/vectors.json"

// ---- vectors.json schema ----

type vectorFile struct {
	Comment           string             `json:"_comment"`
	SchemaVersion     uint16             `json:"schemaVersion"`
	DomainTagPreimage string             `json:"domainTagPreimage"`
	DomainTag         string             `json:"domainTag"`
	Agreement         []agreementVector  `json:"agreement"`
	Commitments       []commitmentVector `json:"commitments"`
	Ordering          []groupVector      `json:"ordering"`
	Distinct          []groupVector      `json:"distinct"`
	Rejection         []rejectionVector  `json:"rejection"`
}

type fieldSpec struct {
	Name     string          `json:"name,omitempty"`
	NameHex  string          `json:"nameHex,omitempty"`
	Type     string          `json:"type"`
	Value    json.RawMessage `json:"value,omitempty"`
	ValueHex string          `json:"valueHex,omitempty"`
}

type agreementVector struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Payload     []fieldSpec `json:"payload"`
	Encoding    string      `json:"encoding"`
	PayloadHash string      `json:"payloadHash"`
}

type commitmentVector struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Payload     []fieldSpec `json:"payload"`
	FabricTxID  string      `json:"fabricTxId"`
	ChannelID   string      `json:"channelId"`
	ChaincodeID string      `json:"chaincodeId"`
	BlockNumber uint64      `json:"blockNumber"`
	Timestamp   uint64      `json:"timestamp"`
	PayloadHash string      `json:"payloadHash"`
	ABIEncoding string      `json:"abiEncoding"`
	Commitment  string      `json:"commitment"`
}

type groupVector struct {
	ID          string        `json:"id"`
	Description string        `json:"description"`
	Payloads    [][]fieldSpec `json:"payloads"`
}

type rejectionVector struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Payload     []fieldSpec `json:"payload"`
}

// buildPayload converts field specs into a Payload via the public
// constructors, so vector rejection semantics match the real API surface.
func buildPayload(specs []fieldSpec) (Payload, error) {
	var p Payload
	for _, s := range specs {
		name := s.Name
		if s.NameHex != "" {
			raw, err := decodeHex(s.NameHex)
			if err != nil {
				return Payload{}, err
			}
			name = string(raw)
		}
		var (
			f   Field
			err error
		)
		switch s.Type {
		case "int":
			var dec string
			if uerr := json.Unmarshal(s.Value, &dec); uerr != nil {
				return Payload{}, fmt.Errorf("vector int value must be a decimal string: %w", uerr)
			}
			v, ok := new(big.Int).SetString(dec, 10)
			if !ok {
				return Payload{}, fmt.Errorf("%w: %q is not a base-10 integer", ErrInvalidValue, dec)
			}
			f, err = IntField(name, v)
		case "bool":
			var b bool
			if uerr := json.Unmarshal(s.Value, &b); uerr != nil {
				return Payload{}, fmt.Errorf("vector bool value: %w", uerr)
			}
			f, err = BoolField(name, b)
		case "string":
			if s.ValueHex != "" {
				raw, herr := decodeHex(s.ValueHex)
				if herr != nil {
					return Payload{}, herr
				}
				f, err = StringField(name, string(raw))
			} else {
				var v string
				if uerr := json.Unmarshal(s.Value, &v); uerr != nil {
					return Payload{}, fmt.Errorf("vector string value: %w", uerr)
				}
				f, err = StringField(name, v)
			}
		case "bytes":
			raw, herr := decodeHex(s.ValueHex)
			if herr != nil {
				return Payload{}, herr
			}
			f, err = BytesField(name, raw)
		default:
			return Payload{}, fmt.Errorf("%w: vector field type %q is not part of the type system", ErrInvalidType, s.Type)
		}
		if err != nil {
			return Payload{}, err
		}
		p.Fields = append(p.Fields, f)
	}
	return p, nil
}

func decodeHex(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

func loadVectors(t *testing.T) *vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(vectorsPath))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return &vf
}

func TestVectors(t *testing.T) {
	vf := loadVectors(t)

	if vf.SchemaVersion != SchemaVersion {
		t.Fatalf("vectors schemaVersion %d != implementation %d", vf.SchemaVersion, SchemaVersion)
	}
	if vf.DomainTagPreimage != domainTagPreimage {
		t.Fatalf("domain tag preimage mismatch: %q", vf.DomainTagPreimage)
	}
	if got := "0x" + hex.EncodeToString(DomainTag[:]); got != vf.DomainTag {
		t.Fatalf("domain tag: vectors %s, implementation %s", vf.DomainTag, got)
	}

	dirty := false

	t.Run("agreement", func(t *testing.T) {
		for i := range vf.Agreement {
			v := &vf.Agreement[i]
			p, err := buildPayload(v.Payload)
			if err != nil {
				t.Errorf("%s: build: %v", v.ID, err)
				continue
			}
			enc, err := Encode(p)
			if err != nil {
				t.Errorf("%s: encode: %v", v.ID, err)
				continue
			}
			hash, err := PayloadHash(p)
			if err != nil {
				t.Errorf("%s: hash: %v", v.ID, err)
				continue
			}
			gotEnc := "0x" + hex.EncodeToString(enc)
			gotHash := "0x" + hex.EncodeToString(hash[:])
			if *update {
				if v.Encoding != gotEnc || v.PayloadHash != gotHash {
					v.Encoding, v.PayloadHash = gotEnc, gotHash
					dirty = true
				}
				continue
			}
			if v.Encoding != gotEnc {
				t.Errorf("%s: encoding mismatch\n want %s\n  got %s", v.ID, v.Encoding, gotEnc)
			}
			if v.PayloadHash != gotHash {
				t.Errorf("%s: payloadHash mismatch\n want %s\n  got %s", v.ID, v.PayloadHash, gotHash)
			}
		}
	})

	t.Run("commitments", func(t *testing.T) {
		for i := range vf.Commitments {
			v := &vf.Commitments[i]
			p, err := buildPayload(v.Payload)
			if err != nil {
				t.Errorf("%s: build: %v", v.ID, err)
				continue
			}
			payloadHash, err := PayloadHash(p)
			if err != nil {
				t.Errorf("%s: payload hash: %v", v.ID, err)
				continue
			}
			txIDBytes, err := decodeHex(v.FabricTxID)
			if err != nil || len(txIDBytes) != 32 {
				t.Errorf("%s: bad fabricTxId", v.ID)
				continue
			}
			var txID [32]byte
			copy(txID[:], txIDBytes)
			c := NewCommitment(txID, v.ChannelID, v.ChaincodeID, v.BlockNumber, v.Timestamp, payloadHash)
			abiEnc, err := c.ABIEncode()
			if err != nil {
				t.Errorf("%s: abi encode: %v", v.ID, err)
				continue
			}
			hash, err := c.Hash()
			if err != nil {
				t.Errorf("%s: commitment hash: %v", v.ID, err)
				continue
			}
			gotABI := "0x" + hex.EncodeToString(abiEnc)
			gotHash := "0x" + hex.EncodeToString(hash[:])
			gotPayloadHash := "0x" + hex.EncodeToString(payloadHash[:])
			if *update {
				if v.ABIEncoding != gotABI || v.Commitment != gotHash || v.PayloadHash != gotPayloadHash {
					v.ABIEncoding, v.Commitment, v.PayloadHash = gotABI, gotHash, gotPayloadHash
					dirty = true
				}
				continue
			}
			if v.PayloadHash != gotPayloadHash {
				t.Errorf("%s: payloadHash mismatch\n want %s\n  got %s", v.ID, v.PayloadHash, gotPayloadHash)
			}
			if v.ABIEncoding != gotABI {
				t.Errorf("%s: abiEncoding mismatch\n want %s\n  got %s", v.ID, v.ABIEncoding, gotABI)
			}
			if v.Commitment != gotHash {
				t.Errorf("%s: commitment mismatch\n want %s\n  got %s", v.ID, v.Commitment, gotHash)
			}
		}
	})

	t.Run("ordering", func(t *testing.T) {
		for _, v := range vf.Ordering {
			hashes := make([][32]byte, 0, len(v.Payloads))
			for j, specs := range v.Payloads {
				p, err := buildPayload(specs)
				if err != nil {
					t.Errorf("%s[%d]: build: %v", v.ID, j, err)
					continue
				}
				h, err := PayloadHash(p)
				if err != nil {
					t.Errorf("%s[%d]: hash: %v", v.ID, j, err)
					continue
				}
				hashes = append(hashes, h)
			}
			for j := 1; j < len(hashes); j++ {
				if hashes[j] != hashes[0] {
					t.Errorf("%s: payload %d hashes differently from payload 0", v.ID, j)
				}
			}
		}
	})

	t.Run("distinct", func(t *testing.T) {
		for _, v := range vf.Distinct {
			hashes := make(map[[32]byte]int)
			for j, specs := range v.Payloads {
				p, err := buildPayload(specs)
				if err != nil {
					t.Errorf("%s[%d]: build: %v", v.ID, j, err)
					continue
				}
				h, err := PayloadHash(p)
				if err != nil {
					t.Errorf("%s[%d]: hash: %v", v.ID, j, err)
					continue
				}
				if prev, dup := hashes[h]; dup {
					t.Errorf("%s: payloads %d and %d collide", v.ID, prev, j)
				}
				hashes[h] = j
			}
		}
	})

	t.Run("rejection", func(t *testing.T) {
		for _, v := range vf.Rejection {
			p, err := buildPayload(v.Payload)
			if err != nil {
				continue // rejected at construction: correct
			}
			if _, err := Encode(p); err == nil {
				t.Errorf("%s (%s): expected rejection, but payload encoded", v.ID, v.Description)
			}
		}
	})

	if *update && dirty {
		out, err := json.MarshalIndent(vf, "", "  ")
		if err != nil {
			t.Fatalf("marshal vectors: %v", err)
		}
		if err := os.WriteFile(filepath.FromSlash(vectorsPath), append(out, '\n'), 0o644); err != nil {
			t.Fatalf("write vectors: %v", err)
		}
		t.Log("vectors.json updated")
	}
}
