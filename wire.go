// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// tagged writes v (a struct) as a JSON object with tag:value first.
func tagged(tag, value string, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	head, _ := json.Marshal(tag)
	val, _ := json.Marshal(value)
	var b bytes.Buffer
	b.WriteByte('{')
	b.Write(head)
	b.WriteByte(':')
	b.Write(val)
	if inner := bytes.TrimSpace(body); len(inner) > 2 {
		b.WriteByte(',')
		b.Write(inner[1 : len(inner)-1])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// tagOf reads the string field tag of a JSON object.
func tagOf(data []byte, tag string) (string, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return "", err
	}
	raw, ok := head[tag]
	if !ok {
		return "", fmt.Errorf("latai: no %q field in %s", tag, truncate(data))
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("latai: %q is not a string: %w", tag, err)
	}
	return s, nil
}

func truncate(b []byte) string {
	if len(b) > 80 {
		return string(b[:80]) + "..."
	}
	return string(b)
}

// TextRequest is a request with a text input.
func TextRequest(pack, text string) *Request {
	return &Request{Pack: pack, Inputs: Inputs{Text: &InputsText{Text: text}}}
}

// MessagesRequest is a request with a chat.
func MessagesRequest(pack string, messages ...Message) *Request {
	return &Request{Pack: pack, Inputs: Inputs{Messages: &InputsMessages{Messages: messages}}}
}

// ImageRequest is a request with an image file (PNG or JPEG bytes) and an instruction after it.
func ImageRequest(pack string, image []byte, prompt string) *Request {
	in := &InputsImage{Data: encodeBase64(image)}
	if prompt != "" {
		in.Prompt = &prompt
	}
	return &Request{Pack: pack, Inputs: Inputs{Image: in}}
}

// AudioRequest is a request with a clip (a WAV file, or raw 16-bit samples with format pcm16).
func AudioRequest(pack string, clip []byte) *Request {
	return &Request{Pack: pack, Inputs: Inputs{Audio: &InputsAudio{Data: encodeBase64(clip)}}}
}

// Text is the text of a text, calls or transcript output ("" for any other kind).
func (r *Response) Text() string {
	switch o := r.Output; {
	case o.Text != nil:
		return o.Text.Text
	case o.Calls != nil:
		return o.Calls.Text
	case o.Transcript != nil:
		return o.Transcript.Text
	}
	return ""
}

// Bytes is raw bytes written as a JSON array of numbers (the wire's `bytes` of a token), not as
// the base64 string Go writes for a []byte.
type Bytes []byte

// MarshalJSON writes the bytes as an array of numbers.
func (b Bytes) MarshalJSON() ([]byte, error) {
	if b == nil {
		return []byte("null"), nil
	}
	out := make([]byte, 0, 2+4*len(b))
	out = append(out, '[')
	for i, v := range b {
		if i > 0 {
			out = append(out, ',')
		}
		out = fmt.Appendf(out, "%d", v)
	}
	return append(out, ']'), nil
}

// UnmarshalJSON reads an array of numbers from 0 to 255.
func (b *Bytes) UnmarshalJSON(data []byte) error {
	var nums []uint8
	if err := json.Unmarshal(data, &nums); err != nil {
		return err
	}
	*b = Bytes(nums)
	return nil
}
