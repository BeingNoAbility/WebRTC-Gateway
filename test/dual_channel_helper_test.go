package test

import (
	"sync/atomic"

	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/mux"
)

// setupClientDualMux creates p2pcontrol and p2pdata DataChannels on clientPC,
// returning the channels, an allOpen channel that closes when both channels open,
// and a function to instantiate the client Mux and attach incoming message handlers.
func setupClientDualMux(clientPC *webrtc.PeerConnection) (controlDC, dataDC *webrtc.DataChannel, allOpenCh chan struct{}, createMux func() *mux.Mux, err error) {
	controlDC, err = clientPC.CreateDataChannel("p2pcontrol", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	dataDC, err = clientPC.CreateDataChannel("p2pdata", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		return nil, nil, nil, nil, err
	}

	allOpenCh = make(chan struct{})
	var openCount atomic.Int32
	onOpen := func() {
		if openCount.Add(1) == 2 {
			close(allOpenCh)
		}
	}
	controlDC.OnOpen(onOpen)
	dataDC.OnOpen(onOpen)

	m := mux.NewDualMux(controlDC, dataDC, true)
	handleMsg := func(msg webrtc.DataChannelMessage) {
		_ = m.HandleIncomingMessage(msg.Data)
	}
	controlDC.OnMessage(handleMsg)
	dataDC.OnMessage(handleMsg)

	createMux = func() *mux.Mux {
		return m
	}

	return controlDC, dataDC, allOpenCh, createMux, nil
}
