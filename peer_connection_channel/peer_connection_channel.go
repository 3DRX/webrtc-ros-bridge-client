package peerconnectionchannel

/*
#cgo LDFLAGS: -L. -lvp8decoder -lvpx -lm
#include "vp8_decoder.h"
*/
import "C"

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"unsafe"

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"gocv.io/x/gocv"
)

type PeerConnectionChannel struct {
	sdpChan           <-chan webrtc.SessionDescription
	sdpReplyChan      chan<- webrtc.SessionDescription
	candidateChan     <-chan webrtc.ICECandidateInit
	pendingCandidates []*webrtc.ICECandidate
	candidatesMux     *sync.Mutex
	peerConnection    *webrtc.PeerConnection
	m                 *webrtc.MediaEngine
	signalCandidate   func(c webrtc.ICECandidateInit) error
}

func registerHeaderExtensionURI(m *webrtc.MediaEngine, uris []string) {
	for _, uri := range uris {
		err := m.RegisterHeaderExtension(
			webrtc.RTPHeaderExtensionCapability{
				URI: uri,
			},
			webrtc.RTPCodecTypeVideo,
			webrtc.RTPTransceiverDirectionRecvonly,
		)
		if err != nil {
			panic(err)
		}
	}
}

func InitPeerConnectionChannel(
	sdpChan chan webrtc.SessionDescription,
	sdpReplyChan chan<- webrtc.SessionDescription,
	candidateChan <-chan webrtc.ICECandidateInit,
	pendingCandidates []*webrtc.ICECandidate,
	candidatesMux *sync.Mutex,
	signalCandidate func(c webrtc.ICECandidateInit) error,
) *PeerConnectionChannel {
	m := &webrtc.MediaEngine{}
	// Register VP8
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, Channels: 0},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	// Register VP9
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, Channels: 0},
		PayloadType:        98,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	// Register H264
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, Channels: 0},
		PayloadType:        102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	registerHeaderExtensionURI(m, []string{
		"urn:ietf:params:rtp-hdrext:toffset",
		"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time",
		"urn:3gpp:video-orientation",
		"http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01",
		"http://www.webrtc.org/experiments/rtp-hdrext/playout-delay",
		"http://www.webrtc.org/experiments/rtp-hdrext/video-content-type",
		"http://www.webrtc.org/experiments/rtp-hdrext/video-timing",
		"http://www.webrtc.org/experiments/rtp-hdrext/color-space",
	})
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	return &PeerConnectionChannel{
		sdpChan:           sdpChan,
		sdpReplyChan:      sdpReplyChan,
		candidateChan:     candidateChan,
		pendingCandidates: pendingCandidates,
		candidatesMux:     candidatesMux,
		peerConnection:    peerConnection,
		m:                 m,
		signalCandidate:   signalCandidate,
	}
}

func handleSignalingMessage(pc *PeerConnectionChannel) {
	for {
		select {
		case sdp := <-pc.sdpChan:
			slog.Info("received SDP", "sdp", sdp.SDP)
			err := pc.peerConnection.SetRemoteDescription(sdp)
			if err != nil {
				panic(err)
			}
			answer, err := pc.peerConnection.CreateAnswer(nil)
			if err != nil {
				panic(err)
			}
			pc.sdpReplyChan <- answer
			err = pc.peerConnection.SetLocalDescription(answer)
			if err != nil {
				panic(err)
			}
			pc.candidatesMux.Lock()
			for _, c := range pc.pendingCandidates {
				onICECandidateErr := pc.signalCandidate(c.ToJSON())
				if onICECandidateErr != nil {
					panic(onICECandidateErr)
				}
			}
			pc.candidatesMux.Unlock()
		case candidate := <-pc.candidateChan:
			err := pc.peerConnection.AddICECandidate(candidate)
			if err != nil {
				panic(err)
			}
			slog.Info("received ICE candidate", "candidate", candidate)
		}
	}
}

func (pc *PeerConnectionChannel) Spin() {
	_, err := pc.peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	)
	if err != nil {
		panic(err)
	}
	pc.peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		pc.candidatesMux.Lock()
		defer pc.candidatesMux.Unlock()
		desc := pc.peerConnection.RemoteDescription()
		if desc == nil {
			pc.pendingCandidates = append(pc.pendingCandidates, c)
		} else if err := pc.signalCandidate(c.ToJSON()); err != nil {
			panic(err)
		}
	})
	pc.peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Info("PeerConnectionChannel: connection state changed", "state", state)
		if state == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure. It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			slog.Info("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}

		if state == webrtc.PeerConnectionStateClosed {
			// PeerConnection was explicitly closed. This usually happens from a DTLS CloseNotify
			slog.Info("Peer Connection has gone to closed exiting")
			os.Exit(0)
		}
	})
	pc.peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		slog.Info("PeerConnectionChannel: received track", "track", track)
		// Create a VP8 codec context
		width, height := C.uint(640), C.uint(480)
		var codec C.vpx_codec_ctx_t
		if C.init_decoder(&codec, width, height) != 0 {
			fmt.Println("Failed to initialize decoder")
			return
		}
		currentFrame := []byte{}
		seenKeyFrame := false
		frameCount := 0
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				fmt.Println("Error reading RTP:", err)
				break
			}
			vp8Packet := codecs.VP8Packet{}
			if _, err := vp8Packet.Unmarshal(packet.Payload); err != nil {
				panic(err)
			}
			isKeyFrame := vp8Packet.Payload[0] & 0x01
			switch {
			case !seenKeyFrame && isKeyFrame == 1:
				continue
			case currentFrame == nil && vp8Packet.S != 1:
				continue
			}
			seenKeyFrame = true
			currentFrame = append(currentFrame, vp8Packet.Payload[0:]...)

			if !packet.Marker {
				continue
			} else if len(currentFrame) == 0 {
				continue
			}

			// Start decoding
			// Decode the VP8 payload
			codecError := C.decode_frame(&codec, (*C.uint8_t)(&currentFrame[0]), C.size_t(len(currentFrame)))
			if codecError != 0 {
				slog.Error("Failed to decode frame", "errorCode", codecError)
				continue
			}
			slog.Info("Current frame size", "size", len(currentFrame))

			var iter C.vpx_codec_iter_t
			for img := C.vpx_codec_get_frame(&codec, &iter); img != nil; img = C.vpx_codec_get_frame(&codec, &iter) {
				slog.Info("Retrieved frame", "width", img.d_w, "height", img.d_h)

				// Create GoCV Mat with the correct size and type
				goImg := gocv.NewMatWithSize(int(img.d_h), int(img.d_w), gocv.MatTypeCV8UC3)
				if goImg.Empty() {
					slog.Error("Failed to create Mat")
					continue
				}
				defer goImg.Close()

				// Get pointer to Mat data
				goImgPtr, err := goImg.DataPtrUint8()
				if err != nil {
					slog.Error("Failed to get Mat data pointer", "error", err)
					continue
				}

				// Copy and convert frame data from YUV to BGR
				C.copy_frame_to_mat(
					img,
					(*C.uchar)(unsafe.Pointer(&goImgPtr[0])),
					img.d_w,
					img.d_h,
				)

				// Save the frame as JPEG
				filename := fmt.Sprintf("frame_%d.jpg", frameCount)
				if ok := gocv.IMWrite(filename, goImg); !ok {
					slog.Error("Failed to write image", "filename", filename)
				} else {
					slog.Info("Saved frame", "filename", filename)
				}

				frameCount++
			}
			currentFrame = nil
		}
	})
	pc.peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
		slog.Info("PeerConnectionChannel: signaling state changed", "state", state)
	})
	pc.peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		slog.Info("ICE Connection State changed",
			"state", connectionState,
			"signalingState", pc.peerConnection.SignalingState(),
			"connectionState", pc.peerConnection.ConnectionState(),
		)

		if connectionState == webrtc.ICEConnectionStateConnected {
			slog.Info("Ctrl+C the remote client to stop the demo")
		} else if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateClosed {
			// Gracefully shutdown the peer connection
			if closeErr := pc.peerConnection.Close(); closeErr != nil {
				panic(closeErr)
			}
			os.Exit(0)
		}
	})
	go handleSignalingMessage(pc)
}
