// pufferfs-source-digest keeps a resumable SHA-256 state for streaming workers.
// Python's hashlib cannot export its state. This small pipe protocol uses Go's
// standard implementation and BinaryMarshaler instead of custom cryptography.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

func run(input io.Reader, output io.Writer) error {
	digest := sha256.New()
	reader := bufio.NewReader(input)
	encoder := json.NewEncoder(output)
	for {
		var header [5]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return errors.New("truncated digest command")
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length > 1<<20 {
			return errors.New("digest command exceeds one MiB")
		}
		data := make([]byte, int(length))
		if _, err := io.ReadFull(reader, data); err != nil {
			return errors.New("truncated digest payload")
		}
		switch header[0] {
		case 'D':
			if _, err := digest.Write(data); err != nil {
				return err
			}
		case 'R':
			if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(data); err != nil {
				return errors.New("invalid saved source digest state")
			}
		case 'S':
			if length != 0 {
				return errors.New("snapshot command has a payload")
			}
			state, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				return err
			}
			if err = encoder.Encode(struct {
				State []byte `json:"state"`
				Hash  string `json:"hash"`
			}{state, fmt.Sprintf("sha256:%x", digest.Sum(nil))}); err != nil {
				return err
			}
		default:
			return errors.New("unknown digest command")
		}
	}
}

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
