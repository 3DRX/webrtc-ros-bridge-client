package peerconnectionchannel

/*
#cgo LDFLAGS: -L. -lvp8decoder -lvpx -lm
#include "vp8_decoder.h"
*/
import "C"

import (
	"log/slog"
	"time"
	"unsafe"

	"github.com/pion/interceptor/pkg/jitterbuffer"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	"gocv.io/x/gocv"
)

type WebmSaver struct {
	vp8Builder     *samplebuilder.SampleBuilder
	videoTimestamp time.Duration

	h264JitterBuffer   *jitterbuffer.JitterBuffer
	lastVideoTimestamp uint32
	codecCtx           C.vpx_codec_ctx_t
	codecCreated       bool
	imgChan            chan<- gocv.Mat
}

func newWebmSaver(imgChan chan<- gocv.Mat) *WebmSaver {
	return &WebmSaver{
		vp8Builder:       samplebuilder.New(200, &codecs.VP8Packet{}, 90000),
		h264JitterBuffer: jitterbuffer.New(),
		imgChan:          imgChan,
		codecCreated:     false,
	}
}

func (s *WebmSaver) Close() {
	if s.codecCreated {
		// TODO close codec
	}
}

func (s *WebmSaver) PushVP8(rtpPacket *rtp.Packet) {
	s.vp8Builder.Push(rtpPacket)

	for {
		sample := s.vp8Builder.Pop()
		if sample == nil {
			return
		}
		// Read VP8 header.
		videoKeyframe := (sample.Data[0]&0x1 == 0)
		if videoKeyframe {
			// Keyframe has frame information.
			raw := uint(sample.Data[6]) | uint(sample.Data[7])<<8 | uint(sample.Data[8])<<16 | uint(sample.Data[9])<<24
			width := int(raw & 0x3FFF)
			height := int((raw >> 16) & 0x3FFF)

			if !s.codecCreated {
				s.InitWriter(width, height)
			}
		}

		// Decode VP8 frame
		codecError := C.decode_frame(&s.codecCtx, (*C.uint8_t)(&sample.Data[0]), C.size_t(len(sample.Data)))
		if codecError != 0 {
			slog.Error("Decode error", "errorCode", codecError)
			continue
		}
		// Get decoded frames
		var iter C.vpx_codec_iter_t
		img := C.vpx_codec_get_frame(&s.codecCtx, &iter)
		if img == nil {
			slog.Error("Failed to get decoded frame")
			continue
		}
		actualWidth := int(img.d_w)
		actualHeight := int(img.d_h)
		goImg := gocv.NewMatWithSize(actualHeight, actualWidth, gocv.MatTypeCV8UC3)
		if goImg.Empty() {
			slog.Error("Failed to create Mat")
			continue
		}
		// Get Mat data pointer
		goImgPtr, err := goImg.DataPtrUint8()
		if err != nil {
			slog.Error("Failed to get Mat data pointer", "error", err)
			continue
		}
		// Convert YUV to BGR
		C.copy_frame_to_mat(
			img,
			(*C.uchar)(unsafe.Pointer(&goImgPtr[0])),
			C.uint(actualWidth),
			C.uint(actualHeight),
		)
		s.imgChan <- goImg
	}
}

func (s *WebmSaver) InitWriter(width, height int) {
	if errCode := C.init_decoder(&s.codecCtx, C.uint(width), C.uint(height)); errCode != 0 {
		slog.Error("failed to initialize decoder", "error", errCode)
	}
	s.codecCreated = true
}
