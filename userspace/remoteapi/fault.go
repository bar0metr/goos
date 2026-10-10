package remoteapi

import "goos/kernel/ipc"

const signerFaultProtocolID = uint64(0x47534947464c5431) // GSIGFLT1

func SignerFaultNotice() ipc.Message {
	return ipc.Message{ProtocolID: signerFaultProtocolID, Version: 1, Kind: ipc.KindNotification}
}

func DecodeSignerFaultNotice(message *ipc.Message) bool {
	return message.ProtocolID == signerFaultProtocolID && message.Version == 1 &&
		message.Kind == ipc.KindNotification && message.Correlation == 0 &&
		message.HandleCount == 0 && message.PayloadSize == 0
}
