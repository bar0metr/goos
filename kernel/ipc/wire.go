package ipc

import (
	"encoding/binary"
	"goos/kernel/handle"
)

const (
	WireHeaderSize = 32
	WireMaxSize    = WireHeaderSize + MaxPayload + MaxAttachedHandles*8
)

var wireMagic = [4]byte{'G', 'I', 'P', 'C'}

func Encode(message Message, target []byte) (int, bool) {
	return EncodeFrom(&message, target)
}

func EncodeFrom(message *Message, target []byte) (int, bool) {
	if message == nil {
		return 0, false
	}
	if !validMessage(message) || message.HandleCount > MaxAttachedHandles {
		return 0, false
	}
	size := WireHeaderSize + int(message.PayloadSize) + int(message.HandleCount)*8
	if len(target) < size {
		return 0, false
	}
	for index := 0; index < size; index++ {
		target[index] = 0
	}
	copy(target[:4], wireMagic[:])
	binary.LittleEndian.PutUint64(target[4:], message.ProtocolID)
	binary.LittleEndian.PutUint16(target[12:], message.Version)
	target[14] = byte(message.Kind)
	binary.LittleEndian.PutUint64(target[16:], message.Correlation)
	binary.LittleEndian.PutUint16(target[24:], message.PayloadSize)
	target[26] = message.HandleCount
	copy(target[WireHeaderSize:], message.Payload[:message.PayloadSize])
	offset := WireHeaderSize + int(message.PayloadSize)
	for index := 0; index < int(message.HandleCount); index++ {
		binary.LittleEndian.PutUint64(target[offset+index*8:], uint64(message.Handles[index]))
	}
	return size, true
}

func Decode(data []byte) (Message, bool) {
	var message Message
	ok := DecodeInto(data, &message)
	return message, ok
}

func DecodeInto(data []byte, message *Message) bool {
	if message == nil {
		return false
	}
	*message = Message{}
	if len(data) < WireHeaderSize || data[0] != wireMagic[0] || data[1] != wireMagic[1] || data[2] != wireMagic[2] || data[3] != wireMagic[3] ||
		data[15] != 0 || binary.LittleEndian.Uint32(data[28:]) != 0 {
		return false
	}
	message.ProtocolID = binary.LittleEndian.Uint64(data[4:])
	message.Version = binary.LittleEndian.Uint16(data[12:])
	message.Kind = Kind(data[14])
	message.Correlation = binary.LittleEndian.Uint64(data[16:])
	message.PayloadSize = binary.LittleEndian.Uint16(data[24:])
	message.HandleCount = data[26]
	if data[27] != 0 || message.PayloadSize > MaxPayload || message.HandleCount > MaxAttachedHandles {
		return false
	}
	size := WireHeaderSize + int(message.PayloadSize) + int(message.HandleCount)*8
	if len(data) != size {
		return false
	}
	copy(message.Payload[:], data[WireHeaderSize:WireHeaderSize+int(message.PayloadSize)])
	offset := WireHeaderSize + int(message.PayloadSize)
	for index := 0; index < int(message.HandleCount); index++ {
		message.Handles[index] = handle.ID(binary.LittleEndian.Uint64(data[offset+index*8:]))
		if message.Handles[index] == 0 {
			return false
		}
	}
	if !validWireMessage(message) {
		return false
	}
	return true
}

func validWireMessage(message *Message) bool {
	return validMessage(message)
}
