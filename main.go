package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/asticode/go-astits"
	"github.com/haivision/srtgo"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

const (
	defaultPort       = 7000
	defaultBufferSize = 1500
)

func main() {
	whipURL := flag.String("whip", "", "WHIP endpoint URL to publish to")
	flag.Parse()

	if *whipURL == "" {
		log.Fatalf("missing required -whip flag")
	}

	// Initialize SRT library and ensure we clean up on exit.
	srtgo.InitSRT()
	defer srtgo.CleanupSRT()

	port := readPort()
	log.Printf("starting SRT listener on :%d", port)

	opts := map[string]string{
		"messageapi":  "1", // Use message API for chunked TS packets.
		"blocking":    "1",
		"payloadsize": "1316",
	}
	listener := srtgo.NewSrtSocket("0.0.0.0", uint16(port), opts)
	if listener == nil {
		log.Fatalf("failed to create SRT listener socket")
	}
	if err := listener.Listen(128); err != nil {
		log.Fatalf("failed to listen on SRT socket: %v", err)
	}

	for {
		conn, addr, err := listener.Accept()
		if err != nil {
			log.Printf("error accepting SRT connection: %v", err)
			continue
		}
		go handleConnection(conn, addr, *whipURL)
	}
}

func readPort() int {
	if v := os.Getenv("PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return defaultPort
}

type srtConn interface {
	Read([]byte) (int, error)
	PacketSize() int
	GetSockOptString(opt int) (string, error)
	Close()
}

func handleConnection(conn srtConn, addr *net.UDPAddr, whipURL string) {
	defer conn.Close()

	streamID, _ := conn.GetSockOptString(srtgo.SRTO_STREAMID)
	streamKey := deriveStreamKey(streamID)
	log.Printf("accepted SRT from %s streamid=%q streamKey=%q", addr.String(), strings.TrimSpace(streamID), streamKey)

	// Bridge SRT messages into a continuous stream for the TS demuxer.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		bufSize := conn.PacketSize()
		if bufSize < 1316 {
			bufSize = 1316
		}
		buf := make([]byte, bufSize)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if _, werr := pw.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	demux := astits.NewDemuxer(context.Background(), pr)
	streamTypes := map[uint16]astits.StreamType{}
	var pipe *pipeline

	for {
		data, err := demux.NextData()
		if err != nil {
			if err != io.EOF {
				log.Printf("demux error for stream %q: %v", streamKey, err)
			}
			return
		}
		if data == nil {
			continue
		}
		if data.PMT != nil {
			for _, es := range data.PMT.ElementaryStreams {
				streamTypes[es.ElementaryPID] = es.StreamType
			}
			log.Printf("stream %q PMT parsed, streams=%d", streamKey, len(streamTypes))
			continue
		}
		if data.PES == nil {
			continue
		}
		st, ok := streamTypes[data.PID]
		if !ok {
			continue
		}
		log.Printf("stream %q received PES pid=%d type=%v bytes=%d", streamKey, data.PID, st, len(data.PES.Data))

		if pipe == nil {
			var err error
			pipe, err = startPipeline(st, streamKey, whipURL)
			if err != nil {
				log.Printf("failed to start pipeline for %q: %v", streamKey, err)
				return
			}
			log.Printf("stream %q forwarding to WHIP %s as %s", streamKey, whipURL, pipe.codec.RTPCodecCapability.MimeType)
		}

		pts := pipe.nextPTS
		if oh := data.PES.Header.OptionalHeader; oh != nil && oh.PTS != nil {
			if oh.PTS.Base > 0 {
				pts = uint64(oh.PTS.Base)
			}
		}
		if err := pipe.writeSample(data.PES.Data, pts); err != nil {
			log.Printf("failed to forward sample for %q: %v", streamKey, err)
			return
		}
	}
}

func deriveStreamKey(streamID string) string {
	s := strings.Trim(streamID, "\x00")
	s = strings.TrimSpace(s)
	if s == "" {
		return "stream"
	}
	// Common StreamID formats include "name:streamkey" or URL query strings; try to keep the key human-friendly.
	parts := strings.Split(s, ":")
	if len(parts) > 1 && parts[len(parts)-1] != "" {
		return parts[len(parts)-1]
	}
	return s
}

func createPeerConnection(codec webrtc.RTPCodecParameters, codecType webrtc.RTPCodecType, streamKey string) (*webrtc.PeerConnection, trackWriter, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(codec, codecType); err != nil {
		return nil, nil, fmt.Errorf("register codec: %w", err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, err
	}

	track, err := webrtc.NewTrackLocalStaticRTP(codec.RTPCodecCapability, "srt", streamKey)
	if err != nil {
		pc.Close()
		return nil, nil, err
	}
	rtpSender, err := pc.AddTrack(track)
	if err != nil {
		pc.Close()
		return nil, nil, err
	}

	go func() {
		defer pc.Close()
		for {
			if _, _, rtcpErr := rtpSender.ReadRTCP(); rtcpErr != nil {
				return
			}
		}
	}()

	return pc, track, nil
}

func sendWhipOffer(ctx context.Context, whipURL, streamKey, offerSDP string) (webrtc.SessionDescription, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, whipURL, strings.NewReader(offerSDP))
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Accept", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+streamKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return webrtc.SessionDescription{}, fmt.Errorf("WHIP server responded %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  string(body),
	}
	return answer, nil
}

type pipeline struct {
	pc        *webrtc.PeerConnection
	track     trackWriter
	payloader rtp.Payloader
	codec     webrtc.RTPCodecParameters
	codecType webrtc.RTPCodecType
	clockRate uint32
	seq       uint16
	ssrc      uint32
	basePTS   uint64
	started   bool
	nextPTS   uint64
}

var pipelineFactory func(astits.StreamType, string, string) (*pipeline, error)

type trackWriter interface {
	WriteRTP(*rtp.Packet) error
}

func startPipeline(st astits.StreamType, streamKey, whipURL string) (*pipeline, error) {
	if pipelineFactory != nil {
		return pipelineFactory(st, streamKey, whipURL)
	}
	var (
		codec     webrtc.RTPCodecParameters
		codecType webrtc.RTPCodecType
		payloader rtp.Payloader
		clockRate uint32
	)
	switch st {
	case astits.StreamTypeH264Video:
		codec = webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeH264,
				ClockRate: 90000,
			},
			PayloadType: 102,
		}
		codecType = webrtc.RTPCodecTypeVideo
		payloader = &codecs.H264Payloader{}
		clockRate = 90000
	default:
		return nil, fmt.Errorf("unsupported stream type %v", st)
	}

	pc, track, err := createPeerConnection(codec, codecType, streamKey)
	if err != nil {
		return nil, err
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, err
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, err
	}
	<-gatherComplete

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	answer, err := sendWhipOffer(ctx, whipURL, streamKey, pc.LocalDescription().SDP)
	if err != nil {
		pc.Close()
		return nil, err
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		pc.Close()
		return nil, err
	}

	connected := make(chan struct{})
	failed := make(chan struct{})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("webrtc state %s", state)
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateClosed {
			select {
			case <-failed:
			default:
				close(failed)
			}
		}
	})

	select {
	case <-connected:
	case <-failed:
		pc.Close()
		return nil, fmt.Errorf("webRTC connection failed")
	}

	return &pipeline{
		pc:        pc,
		track:     track,
		payloader: payloader,
		codec:     codec,
		codecType: codecType,
		clockRate: clockRate,
		seq:       uint16(rand.Uint32()),
		ssrc:      rand.Uint32(),
	}, nil
}

func (p *pipeline) writeSample(payload []byte, pts uint64) error {
	if !p.started {
		p.basePTS = pts
		p.started = true
	}
	if pts == 0 {
		pts = p.basePTS + uint64(p.clockRate/30)
	}
	ts := uint32((pts - p.basePTS) & 0xFFFFFFFF)
	nalus := splitAnnexBNalus(payload)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		packets := p.payloader.Payload(1200, nalu)
		for _, pl := range packets {
			p.seq++
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: p.seq,
					Timestamp:      ts,
					SSRC:           p.ssrc,
					PayloadType:    uint8(p.codec.PayloadType),
				},
				Payload: pl,
			}
			if err := p.track.WriteRTP(pkt); err != nil {
				return err
			}
		}
	}
	p.nextPTS = pts + uint64(p.clockRate/30)
	return nil
}

func splitAnnexBNalus(b []byte) [][]byte {
	var out [][]byte
	i := 0
	for i < len(b) {
		if i+3 < len(b) && b[i] == 0x00 && b[i+1] == 0x00 && ((b[i+2] == 0x01) || (b[i+2] == 0x00 && b[i+3] == 0x01)) {
			j := i + 3
			if b[i+2] == 0x00 {
				j = i + 4
			}
			start := j
			i = j
			for i+3 < len(b) {
				if b[i] == 0x00 && b[i+1] == 0x00 && ((b[i+2] == 0x01) || (b[i+2] == 0x00 && b[i+3] == 0x01)) {
					break
				}
				i++
			}
			if i+3 >= len(b) {
				out = append(out, b[start:])
			} else {
				out = append(out, b[start:i])
			}
		} else {
			i++
		}
	}
	return out
}
