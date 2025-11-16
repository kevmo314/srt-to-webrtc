package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/haivision/srtgo"
	"github.com/pion/rtp"
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
		"messageapi": "1", // Ensure we preserve RTP packet boundaries.
		"blocking":   "1",
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

func handleConnection(conn *srtgo.SrtSocket, addr *net.UDPAddr, whipURL string) {
	defer conn.Close()

	streamID, _ := conn.GetSockOptString(srtgo.SRTO_STREAMID)
	streamKey := deriveStreamKey(streamID)
	log.Printf("accepted SRT from %s streamid=%q streamKey=%q", addr.String(), strings.TrimSpace(streamID), streamKey)

	// Read the first RTP packet so we can infer codec parameters.
	firstBuf := make([]byte, defaultBufferSize)
	n, err := conn.Read(firstBuf)
	if err != nil {
		log.Printf("failed reading initial packet from %s: %v", addr.String(), err)
		return
	}
	firstPkt := &rtp.Packet{}
	if err := firstPkt.Unmarshal(firstBuf[:n]); err != nil {
		log.Printf("failed to parse RTP from %s: %v", addr.String(), err)
		return
	}

	codec, codecType, err := codecForPacket(firstPkt)
	if err != nil {
		log.Printf("could not determine codec for %s: %v", addr.String(), err)
		return
	}

	pc, track, err := createPeerConnection(codec, codecType, streamKey)
	if err != nil {
		log.Printf("failed to build WebRTC transport for %s: %v", addr.String(), err)
		return
	}
	defer pc.Close()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Printf("failed to create offer: %v", err)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		log.Printf("failed to set local description: %v", err)
		return
	}
	<-gatherComplete

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	answer, err := sendWhipOffer(ctx, whipURL, streamKey, pc.LocalDescription().SDP)
	if err != nil {
		log.Printf("failed to publish WHIP session: %v", err)
		return
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		log.Printf("failed to set remote description: %v", err)
		return
	}

	connected := make(chan struct{})
	failed := make(chan struct{})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
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
		log.Printf("webRTC connection failed for stream %q", streamKey)
		return
	}

	log.Printf("stream %q forwarding to WHIP %s as %s", streamKey, whipURL, codec.RTPCodecCapability.MimeType)

	rtpSize := defaultBufferSize
	if rtpSize < conn.PacketSize() {
		rtpSize = conn.PacketSize()
	}

	// Forward the first packet that we used for sniffing.
	if err := track.WriteRTP(firstPkt); err != nil {
		log.Printf("failed to forward first packet: %v", err)
		return
	}

	buf := make([]byte, rtpSize)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Printf("read error for stream %q: %v", streamKey, err)
			break
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			log.Printf("invalid RTP packet for stream %q: %v", streamKey, err)
			continue
		}
		if err := track.WriteRTP(pkt); err != nil {
			log.Printf("failed to write RTP for stream %q: %v", streamKey, err)
			break
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

func codecForPacket(pkt *rtp.Packet) (webrtc.RTPCodecParameters, webrtc.RTPCodecType, error) {
	var cap webrtc.RTPCodecCapability
	codecType := webrtc.RTPCodecTypeVideo
	switch pkt.PayloadType {
	case 111, 110:
		cap = webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		}
		codecType = webrtc.RTPCodecTypeAudio
	case 96, 97, 98, 127:
		if isLikelyH264(pkt.Payload) {
			cap = webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeH264,
				ClockRate: 90000,
			}
		} else {
			cap = webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeVP8,
				ClockRate: 90000,
			}
		}
	default:
		if isLikelyH264(pkt.Payload) {
			cap = webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeH264,
				ClockRate: 90000,
			}
		} else {
			return webrtc.RTPCodecParameters{}, codecType, fmt.Errorf("unknown codec payload type %d", pkt.PayloadType)
		}
	}

	return webrtc.RTPCodecParameters{
		RTPCodecCapability: cap,
		PayloadType:        webrtc.PayloadType(pkt.PayloadType),
	}, codecType, nil
}

func isLikelyH264(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	nalType := payload[0] & 0x1F
	return nalType > 0 && nalType < 24
}

func createPeerConnection(codec webrtc.RTPCodecParameters, codecType webrtc.RTPCodecType, streamKey string) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP, error) {
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
