package signalingchannel

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/3DRX/webrtc-ros-bridge/config"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"golang.org/x/exp/rand"
)

type SignalingChannel struct {
	cfg           *config.Config
	topicIdx      int
	recv          chan []byte
	c             *websocket.Conn
	sdpChan       chan<- webrtc.SessionDescription
	sdpReplyChan  <-chan webrtc.SessionDescription
	candidateChan chan<- webrtc.ICECandidateInit
}

type signalingResponse struct {
	Sdp  string
	Type string
}

type ICECandidateJSON struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdp_mid"`
	SDPMLineIndex uint16 `json:"sdp_mline_index"`
	Type          string `json:"type"`
}

func InitSignalingChannel(
	cfg *config.Config,
	topicIdx int,
	sdpChan chan webrtc.SessionDescription,
	sdpReplyChan <-chan webrtc.SessionDescription,
	candidateChan chan<- webrtc.ICECandidateInit,
) *SignalingChannel {
	return &SignalingChannel{
		cfg:           cfg,
		topicIdx:      topicIdx,
		recv:          make(chan []byte),
		c:             nil,
		sdpChan:       sdpChan,
		sdpReplyChan:  sdpReplyChan,
		candidateChan: candidateChan,
	}
}

func newStreamId() string {
	return "webrtc_ros-stream-" + strconv.Itoa(rand.Intn(1000000000))
}

func (s *SignalingChannel) composeActions() map[string]interface{} {
	streamId := newStreamId()
	action := map[string]interface{}{
		"type": "configure",
		"actions": []map[string]interface{}{
			{
				"type": "add_stream",
				"id":   streamId,
			},
			{
				"type":      "add_video_track",
				"stream_id": streamId,
				"id":        streamId + "/subscribed_video",
				"src":       "ros_image:/" + s.cfg.Topics[s.topicIdx].NameIn,
			},
		},
	}
	return action
}

func toTextMessage(data map[string]interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return jsonData, nil
}

func (s *SignalingChannel) SignalCandidate(candidate webrtc.ICECandidateInit) error {
	candidateMsg := map[string]interface{}{
		"type":            "ice_candidate",
		"candidate":       candidate.Candidate,
		"sdp_mid":         candidate.SDPMid,
		"sdp_mline_index": candidate.SDPMLineIndex,
	}
	payload, err := toTextMessage(candidateMsg)
	if err != nil {
		slog.Error("marshal error", "error", err)
	}
	s.c.WriteMessage(websocket.TextMessage, payload)
	slog.Info("send candidate", "candidate", string(payload))
	return nil
}

func (s *SignalingChannel) Spin() {
	u := url.URL{Scheme: "ws", Host: s.cfg.Addr, Path: "/webrtc"}
	slog.Info("start spinning", "url", u.String())
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		panic(err)
	}
	s.c = c
	defer c.Close()
	go func() {
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				slog.Error("recv error", "err", err)
				return
			}
			s.recv <- message
		}
	}()
	slog.Info("dial success")

	cfgMessage, err := toTextMessage(s.composeActions())
	if err != nil {
		slog.Error("compose message error", "error", err)
		return
	}
	c.WriteMessage(websocket.TextMessage, cfgMessage)
	slog.Info("send configure message")
	recvRaw := <-s.recv
	sdp := webrtc.SessionDescription{}
	err = json.Unmarshal(recvRaw, &sdp)
	if err != nil {
		slog.Error("unmarshal error", "error", err)
		return
	}
	s.sdpChan <- sdp
	slog.Info("recv sdp")
	answer := <-s.sdpReplyChan // await answer from peer connection
	// find "m=video 0 UDP/TLS/RTP/SAVPF 96 97 98 99 100 101" in SDP
	// and turn it into "m=video 9 UDP/TLS/RTP/SAVPF 96 97 98 99 100 101"
	answer.SDP = strings.Replace(answer.SDP, "m=video 0", "m=video 9", 1)
	payload, err := json.Marshal(answer)
	if err != nil {
		slog.Error("marshal error", "error", err)
	}
	c.WriteMessage(websocket.TextMessage, payload)
	slog.Info("send answer")
	for {
		candidateRaw := <-s.recv
		candidateJSON := ICECandidateJSON{}
		err := json.Unmarshal(candidateRaw, &candidateJSON)
		if err != nil {
			slog.Error("unmarshal error", "error", err)
			continue
		}
		slog.Info("recv candidate", "candidate", candidateJSON)
		iceCandidate := webrtc.ICECandidateInit{
			Candidate:     candidateJSON.Candidate,
			SDPMid:        &candidateJSON.SDPMid,
			SDPMLineIndex: &candidateJSON.SDPMLineIndex,
		}
		s.candidateChan <- iceCandidate
	}
}
