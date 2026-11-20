package graph

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Property values are JSON-friendly scalars: string, bool, int64, float64.
// They are serialized into the inline property area of a record (340 bytes for
// nodes, 288 for relationships). Layout: u8 count, then per property
// (u8 keyLen, key, u8 type, value). Types: 1 string (u32 len + bytes),
// 2 bool (u8), 3 int (varint), 4 float (8 bytes).
//
// Keys are sorted so the encoding is byte-deterministic; the unique-constraint
// index keys on that encoding.

const (
	ptString = 1
	ptBool   = 2
	ptInt    = 3
	ptFloat  = 4
)

// Props maps property names to scalar values.
type Props map[string]any

func (p Props) Clone() Props {
	if p == nil {
		return nil
	}
	out := make(Props, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// normalize converts the broad set of Go numeric types callers pass in into the
// four supported value kinds.
func normalize(v any) (any, error) {
	switch tv := v.(type) {
	case string, bool, int64, float64:
		return tv, nil
	case int:
		return int64(tv), nil
	case int32:
		return int64(tv), nil
	case uint32:
		return int64(tv), nil
	case uint64:
		if tv > math.MaxInt64 {
			return nil, fmt.Errorf("uint64 value too large")
		}
		return int64(tv), nil
	case float32:
		return float64(tv), nil
	case nil:
		return nil, fmt.Errorf("nil property value not supported")
	default:
		return nil, fmt.Errorf("unsupported property type %T", v)
	}
}

func normalizeProps(p Props) (Props, error) {
	if len(p) == 0 {
		return nil, nil
	}
	out := make(Props, len(p))
	for k, v := range p {
		if len(k) > 255 {
			return nil, fmt.Errorf("property key %q longer than 255 bytes", k)
		}
		nv, err := normalize(v)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", k, err)
		}
		out[k] = nv
	}
	return out, nil
}

func encodeProps(p Props) ([]byte, error) {
	if len(p) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf := make([]byte, 0, 64)
	buf = append(buf, byte(len(keys)))
	for _, k := range keys {
		buf = append(buf, byte(len(k)))
		buf = append(buf, k...)
		switch tv := p[k].(type) {
		case string:
			buf = append(buf, ptString)
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(tv)))
			buf = append(buf, tv...)
		case bool:
			buf = append(buf, ptBool)
			if tv {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		case int64:
			buf = append(buf, ptInt)
			buf = binary.AppendVarint(buf, tv)
		case float64:
			buf = append(buf, ptFloat)
			buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(tv))
		default:
			return nil, fmt.Errorf("property %q: unnormalized type %T", k, p[k])
		}
	}
	return buf, nil
}

func decodeProps(b []byte) (Props, error) {
	if len(b) == 0 {
		return nil, nil
	}
	p := make(Props)
	i := 0
	n := int(b[i])
	i++
	for k := 0; k < n; k++ {
		if i >= len(b) {
			return nil, fmt.Errorf("corrupt props: truncated key length")
		}
		kl := int(b[i])
		i++
		if i+kl > len(b) {
			return nil, fmt.Errorf("corrupt props: truncated key")
		}
		key := string(b[i : i+kl])
		i += kl
		if i >= len(b) {
			return nil, fmt.Errorf("corrupt props: missing type")
		}
		typ := b[i]
		i++
		switch typ {
		case ptString:
			if i+4 > len(b) {
				return nil, fmt.Errorf("corrupt props: truncated string length")
			}
			l := int(binary.LittleEndian.Uint32(b[i:]))
			i += 4
			if i+l > len(b) {
				return nil, fmt.Errorf("corrupt props: truncated string")
			}
			p[key] = string(b[i : i+l])
			i += l
		case ptBool:
			if i >= len(b) {
				return nil, fmt.Errorf("corrupt props: truncated bool")
			}
			p[key] = b[i] == 1
			i++
		case ptInt:
			v, m := binary.Varint(b[i:])
			if m <= 0 {
				return nil, fmt.Errorf("corrupt props: bad int")
			}
			i += m
			p[key] = v
		case ptFloat:
			if i+8 > len(b) {
				return nil, fmt.Errorf("corrupt props: truncated float")
			}
			p[key] = math.Float64frombits(binary.LittleEndian.Uint64(b[i:]))
			i += 8
		default:
			return nil, fmt.Errorf("corrupt props: unknown type %d", typ)
		}
	}
	return p, nil
}

// encodeTypedValue encodes a single scalar in the same form used inside props,
// so constraint index keys distinguish "1" (string) from 1 (int).
func encodeTypedValue(v any) ([]byte, error) {
	buf := []byte{0} // placeholder for count, unused by decodeTypedValue
	switch tv := v.(type) {
	case string:
		buf = append(buf, ptString)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(tv)))
		buf = append(buf, tv...)
	case bool:
		buf = append(buf, ptBool)
		if tv {
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
	case int64:
		buf = append(buf, ptInt)
		buf = binary.AppendVarint(buf, tv)
	case float64:
		buf = append(buf, ptFloat)
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(tv))
	default:
		return nil, fmt.Errorf("unsupported value type %T", v)
	}
	return buf, nil
}
