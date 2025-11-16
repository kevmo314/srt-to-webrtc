package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/haivision/srtgo"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestMain(m *testing.M) {
	srtgo.InitSRT()
	code := m.Run()
	srtgo.CleanupSRT()
	os.Exit(code)
}

func TestEndToEndForward(t *testing.T) {
	streamKey := "streamkey123"
	authHeaderCh := make(chan string, 1)
	rtpReceived := make(chan *rtp.Packet, 1)

	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		authHeaderCh <- r.Header.Get("Authorization")

		offer := webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  string(body),
		}

		m := &webrtc.MediaEngine{}
		if err := m.RegisterDefaultCodecs(); err != nil {
			t.Fatalf("register codecs: %v", err)
		}
		api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
		pc, err := api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatalf("new pc: %v", err)
		}
		pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			go func() {
				for {
					pkt, _, err := tr.ReadRTP()
					if err != nil {
						return
					}
					rtpReceived <- pkt
				}
			}()
		})

		if err := pc.SetRemoteDescription(offer); err != nil {
			t.Fatalf("set remote: %v", err)
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			t.Fatalf("create answer: %v", err)
		}
		gather := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(answer); err != nil {
			t.Fatalf("set local: %v", err)
		}
		<-gather

		w.Header().Set("Content-Type", "application/sdp")
		_, _ = w.Write([]byte(pc.LocalDescription().SDP))
	}))
	defer ws.Close()

	port := pickPort(t)
	opts := map[string]string{
		"messageapi": "1",
		"blocking":   "1",
	}
	listener := srtgo.NewSrtSocket("127.0.0.1", uint16(port), opts)
	if listener == nil {
		t.Fatalf("listener nil")
	}
	if err := listener.Listen(1); err != nil {
		t.Fatalf("listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, addr, err := listener.Accept()
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		handleConnection(conn, addr, ws.URL)
	}()

	caller := srtgo.NewSrtSocket("127.0.0.1", uint16(port), map[string]string{
		"messageapi": "1",
		"blocking":   "1",
		"streamid":   streamKey,
	})
	if caller == nil {
		t.Fatalf("caller nil")
	}
	if err := caller.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 1,
			Timestamp:      555,
			SSRC:           99,
		},
		Payload: []byte{0x65, 0x88, 0x84},
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := caller.Write(raw); err != nil {
		t.Fatalf("write: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatalf("timeout waiting for Authorization header")
	case auth := <-authHeaderCh:
		if auth != "Bearer "+streamKey {
			t.Fatalf("unexpected auth header %q", auth)
		}
	}

	select {
	case <-ctx.Done():
		t.Fatalf("timeout waiting for RTP forward")
	case got := <-rtpReceived:
		if got.PayloadType != pkt.PayloadType {
			t.Fatalf("unexpected payload type %d", got.PayloadType)
		}
	}
	caller.Close()
	<-done
}

func TestDeriveStreamKey(t *testing.T) {
	if got := deriveStreamKey("foo:bar"); got != "bar" {
		t.Fatalf("expected bar, got %s", got)
	}
	if got := deriveStreamKey("solo"); got != "solo" {
		t.Fatalf("expected solo, got %s", got)
	}
	if got := deriveStreamKey(""); got != "stream" {
		t.Fatalf("expected default stream, got %s", got)
	}
}

func pickPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
