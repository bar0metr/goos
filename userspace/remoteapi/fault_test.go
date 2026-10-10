package remoteapi

import "testing"

func TestSignerFaultNoticeRejectsAttachments(t *testing.T) {
	message := SignerFaultNotice()
	if !DecodeSignerFaultNotice(&message) {
		t.Fatal("valid signer fault notice rejected")
	}
	message.HandleCount = 1
	message.Handles[0] = 7
	if DecodeSignerFaultNotice(&message) {
		t.Fatal("signer fault notice accepted a handle")
	}
}
