package protobuf

import (
	"github.com/golang/protobuf/proto"
)

//go:generate protoc --go_out=. ./chat_message.proto ./application_metadata_message.proto ./membership_update_message.proto

func Unmarshal(payload []byte) (*ApplicationMetadataMessage, error) {
	var message ApplicationMetadataMessage
	err := proto.Unmarshal(payload, &message)
	if err != nil {
		return nil, err
	}

	return &message, nil
}
