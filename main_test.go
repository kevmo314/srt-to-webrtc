package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/asticode/go-astits"
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
	rtpReceived := make(chan struct{}, 1)
	trackSeen := make(chan struct{}, 1)

	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		authHeaderCh <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ws.Close()

	prevFactory := pipelineFactory
	pipelineFactory = func(st astits.StreamType, streamKey, whipURL string) (*pipeline, error) {
		t.Logf("stub pipeline invoked for %v", st)
		if st != astits.StreamTypeH264Video {
			return nil, fmt.Errorf("unsupported stream type")
		}
		_, _ = sendWhipOffer(context.Background(), whipURL, streamKey, "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=-\r\n")
		return &pipeline{
			payloader: nopPayloader{},
			codec: webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
				PayloadType:        102,
			},
			codecType: webrtc.RTPCodecTypeVideo,
			clockRate: 90000,
			seq:       1,
			ssrc:      1,
			track: trackWriterFunc(func(pkt *rtp.Packet) error {
				select {
				case trackSeen <- struct{}{}:
				default:
				}
				rtpReceived <- struct{}{}
				return nil
			}),
		}, nil
	}
	defer func() { pipelineFactory = prevFactory }()

	pr, pw := io.Pipe()
	mockConn := &mockSRTConn{r: pr, streamID: streamKey}
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleConnection(mockConn, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, ws.URL)
	}()

	time.Sleep(50 * time.Millisecond)
	tsData := buildTestTS(t)
	t.Logf("ts data bytes=%d", len(tsData))
	if _, err := pw.Write(tsData); err != nil {
		t.Fatalf("write: %v", err)
	}
	pw.Close()

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
		t.Fatalf("timeout waiting for track callback")
	case <-trackSeen:
	}
	select {
	case <-ctx.Done():
		t.Fatalf("timeout waiting for RTP forward")
	case <-rtpReceived:
	}
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

func buildTestTS(t *testing.T) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	mux := astits.NewMuxer(context.Background(), buf)
	if err := mux.AddElementaryStream(astits.PMTElementaryStream{ElementaryPID: 256, StreamType: astits.StreamTypeH264Video}); err != nil {
		t.Fatalf("add es: %v", err)
	}
	mux.SetPCRPID(256)
	if n, err := mux.WriteTables(); err != nil {
		t.Fatalf("write tables: %v", err)
	} else {
		t.Logf("tables bytes=%d", n)
	}
	if buf.Len() == 0 {
		t.Fatalf("no data after tables")
	}

	// Simple IDR NALU 0x65...
	nalu := []byte{0x00, 0x00, 0x01, 0x65, 0x88, 0x84}
	pts := uint64(90000)
	if _, err := mux.WriteData(&astits.MuxerData{
		PID: 256,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				OptionalHeader: &astits.PESOptionalHeader{
					MarkerBits:      2,
					PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
					PTS:             &astits.ClockReference{Base: int64(pts)},
				},
			},
			Data: nalu,
		},
	}); err != nil {
		t.Fatalf("write pes: %v", err)
	}
	t.Logf("ts total bytes=%d", buf.Len())
	if buf.Len()%astits.MpegTsPacketSize != 0 {
		t.Fatalf("ts buffer not aligned: %d", buf.Len())
	}
	// Sanity demux to ensure stream types are present.
	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()))
	seenPMT := false
	seenPES := false
	for i := 0; i < 10; i++ {
		d, err := dmx.NextData()
		if err != nil {
			if errors.Is(err, io.EOF) || err.Error() == "astits: no more packets" {
				break
			}
			t.Fatalf("demux sanity: %v", err)
		}
		if d == nil {
			continue
		}
		if d.PMT != nil {
			seenPMT = true
		}
		if d.PES != nil {
			seenPES = true
		}
	}
	if !seenPMT || !seenPES {
		t.Fatalf("missing PMT/PES in test stream pmt=%v pes=%v", seenPMT, seenPES)
	}
	return buf.Bytes()
}

type mockSRTConn struct {
	r        io.ReadCloser
	streamID string
}

func (m *mockSRTConn) Read(b []byte) (int, error) {
	return m.r.Read(b)
}

func (m *mockSRTConn) PacketSize() int {
	return 1500
}

func (m *mockSRTConn) GetSockOptString(opt int) (string, error) {
	return m.streamID, nil
}

func (m *mockSRTConn) Close() {
	_ = m.r.Close()
}

type trackWriterFunc func(*rtp.Packet) error

func (f trackWriterFunc) WriteRTP(p *rtp.Packet) error {
	return f(p)
}

type nopPayloader struct{}

func (nopPayloader) Payload(mtu uint16, payload []byte) [][]byte {
	return [][]byte{payload}
}
