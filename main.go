package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/nareix/joy5/av"
	"github.com/nareix/joy5/codec/aac"
	"github.com/nareix/joy5/codec/h264"
	"github.com/nareix/joy5/format/flv"
	"github.com/notedit/resample"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
)

var startBytes = []byte{0x00, 0x00, 0x00, 0x01}

type Transcode struct {
	inSampleFormat  resample.SampleFormat
	outSampleFormat resample.SampleFormat
	enc             *resample.AudioEncoder
	dec             *resample.AudioDecoder
	timelien        *resample.Timeline
	inChannels      int
	outChannels     int
	outbitrate      int
	inSampleRate    int
	outSampleRate   int
}

func (t *Transcode) Setup() error {
	dec, err := resample.NewAudioDecoder("libopus")
	if err != nil {
		return err
	}
	dec.SetSampleRate(t.inSampleRate)
	dec.SetSampleFormat(t.inSampleFormat)
	dec.SetChannels(t.inChannels)
	err = dec.Setup()
	if err != nil {
		return err
	}
	t.dec = dec
	enc, err := resample.NewAudioEncoder("aac")
	if err != nil {
		return err
	}
	enc.SetSampleRate(t.outSampleRate)
	enc.SetSampleFormat(t.outSampleFormat)
	enc.SetChannels(t.outChannels)
	enc.SetBitrate(t.outbitrate)
	err = enc.Setup()
	if err != nil {
		return err
	}
	t.enc = enc
	return nil
}

func (t *Transcode) SetInSampleRate(samplerate int) error {
	t.inSampleRate = samplerate
	return nil
}

func (t *Transcode) SetInChannels(channels int) error {
	t.inChannels = channels
	return nil
}

func (t *Transcode) SetInSampleFormat(sampleformat resample.SampleFormat) error {
	t.inSampleFormat = sampleformat
	return nil
}

func (t *Transcode) SetOutSampleRate(samplerate int) error {
	t.outSampleRate = samplerate
	return nil
}

func (t *Transcode) SetOutChannels(channels int) error {
	t.outChannels = channels
	return nil
}

func (t *Transcode) SetOutSampleFormat(sampleformat resample.SampleFormat) error {
	t.outSampleFormat = sampleformat
	return nil
}

func (t *Transcode) SetOutBitrate(bitrate int) error {
	t.outbitrate = bitrate
	return nil
}

func (t *Transcode) Do(data []byte) (out [][]byte, err error) {

	var frame resample.AudioFrame
	var ok bool
	if ok, frame, err = t.dec.Decode(data); err != nil {
		return
	}

	if !ok {
		fmt.Println("does not get one frame")
		return
	}

	if out, err = t.enc.Encode(frame); err != nil {
		return
	}

	return
}

func (t *Transcode) Close() {
	t.enc.Close()
	t.dec.Close()
}

type RTPJitter struct {
	clockrate    uint32
	cap          uint16
	packetsCount uint32
	nextSeqNum   uint16
	packets      []*rtp.Packet
	packetsSeqs  []uint16

	lastTime uint32
	nextTime uint32

	maxWaitTime uint32
	clockInMS   uint32
}

// cap maybe 512 or 1024 or more
func NewJitter(cap uint16, clockrate uint32) *RTPJitter {
	jitter := &RTPJitter{}
	jitter.packets = make([]*rtp.Packet, cap)
	jitter.packetsSeqs = make([]uint16, cap)
	jitter.cap = cap
	jitter.clockrate = clockrate
	jitter.clockInMS = clockrate / 1000
	jitter.maxWaitTime = 100
	return jitter
}

func (self *RTPJitter) Add(packet *rtp.Packet) bool {

	idx := packet.SequenceNumber % self.cap
	self.packets[idx] = packet
	self.packetsSeqs[idx] = packet.SequenceNumber

	if self.packetsCount == 0 {
		self.nextSeqNum = packet.SequenceNumber - 1
		self.nextTime = packet.Timestamp
	}

	self.lastTime = packet.Timestamp
	self.packetsCount++
	return true
}

func (self *RTPJitter) SetMaxWaitTime(wait uint32) {
	self.maxWaitTime = wait
}

func (self *RTPJitter) GetOrdered() (out []*rtp.Packet) {
	nextSeq := self.nextSeqNum + 1
	for {
		idx := nextSeq % self.cap
		if self.packetsSeqs[idx] != nextSeq {
			// if we reach max wait time
			if (self.lastTime - self.nextTime) > self.maxWaitTime*self.clockInMS {
				nextSeq++
				continue
			}
			break
		}
		packet := self.packets[idx]
		out = append(out, packet)
		self.nextTime = packet.Timestamp
		self.nextSeqNum = nextSeq
		nextSeq++
	}
	return
}

type RTPDepacketizer struct {
	frame         []byte
	timestamp     uint32
	h264Unmarshal *codecs.H264Packet
}

func NewDepacketizer() *RTPDepacketizer {
	return &RTPDepacketizer{
		frame:         make([]byte, 0),
		h264Unmarshal: &codecs.H264Packet{},
	}
}

func (self *RTPDepacketizer) AddPacket(pkt *rtp.Packet) ([]byte, uint32) {

	ts := pkt.Timestamp

	if self.timestamp != ts {
		self.frame = make([]byte, 0)
	}

	self.timestamp = ts

	buf, _ := self.h264Unmarshal.Unmarshal(pkt.Payload)

	self.frame = append(self.frame, buf...)

	if !pkt.Marker {
		return nil, 0
	}

	return self.frame, self.timestamp
}

func test(c *gin.Context) {
	c.String(200, "Hello World")
}

func index(c *gin.Context) {
	c.HTML(200, "index.html", gin.H{})
}

type Recorder struct {
	audioFirst       bool
	videoFirst       bool
	flvfile          *os.File
	muxer            *flv.Muxer
	audiojitter      *RTPJitter
	videojitter      *RTPJitter
	h264Unmarshal    *codecs.H264Packet
	depacketizer     *RTPDepacketizer
	h264decodeConfig av.Packet
	aacdecodeConfig  av.Packet
	startTime        time.Time
	trans            *Transcode

	timeline *resample.Timeline

	// order the packets
	timestamps []uint32
	frames     map[uint32][]*av.Packet
}

func newRecorder(filename string) *Recorder {
	file, err := os.Create(filename)
	if err != nil {
		panic(err)
	}

	muxer := flv.NewMuxer(file)
	muxer.HasAudio = true
	muxer.HasVideo = true
	muxer.Publishing = true
	muxer.WriteFileHeader()

	trans := &Transcode{}

	trans.SetInSampleRate(48000)
	trans.SetInChannels(2)
	trans.SetInSampleFormat(resample.S16)
	trans.SetOutChannels(2)
	trans.SetOutSampleFormat(resample.FLTP)
	trans.SetOutSampleRate(48000)
	trans.SetOutBitrate(48000)

	err = trans.Setup()
	if err != nil {
		fmt.Println(err)
	}

	aacconfig := aac.MPEG4AudioConfig{
		SampleRate:      48000,
		ChannelLayout:   aac.CH_STEREO,
		ObjectType:      2, //lowtype
		SampleRateIndex: 3, //48000
		ChannelConfig:   2, //left&right
	}

	aacCodec := &aac.Codec{
		Config: aacconfig,
	}

	h264decodeConfig := av.Packet{
		Type:       av.H264DecoderConfig,
		IsKeyFrame: true,
		H264:       h264.NewCodec(),
	}

	aacdecodeConfig := av.Packet{
		Type: av.AACDecoderConfig,
		AAC:  aacCodec,
	}

	return &Recorder{
		flvfile:          file,
		muxer:            muxer,
		audiojitter:      NewJitter(512, 48000),
		videojitter:      NewJitter(512, 90000),
		h264Unmarshal:    &codecs.H264Packet{},
		depacketizer:     NewDepacketizer(),
		h264decodeConfig: h264decodeConfig,
		aacdecodeConfig:  aacdecodeConfig,
		startTime:        time.Now(),
		trans:            trans,
		timeline:         &resample.Timeline{},
	}
}

func (r *Recorder) PushAudio(pkt *rtp.Packet) {

	if !r.audioFirst {

		configBuffer := bytes.NewBuffer([]byte{})
		err := aac.WriteMPEG4AudioConfig(configBuffer, r.aacdecodeConfig.AAC.Config)

		if err != nil {
			fmt.Println(err)
		}

		r.aacdecodeConfig.AAC.ConfigBytes = configBuffer.Bytes()
		r.aacdecodeConfig.Data = configBuffer.Bytes()

		r.muxer.WritePacket(r.aacdecodeConfig)

		r.audioFirst = true
	}

	r.audiojitter.Add(pkt)

	_pkts := r.audiojitter.GetOrdered()

	for _, _pkt := range _pkts {
		pkts, err := r.trans.Do(_pkt.Payload)
		if err != nil {
			fmt.Println(err)
			continue
		}

		for _, pktdata := range pkts {

			duration := time.Since(r.startTime)
			avpkt := av.Packet{
				Type:  av.AAC,
				Time:  duration,
				CTime: duration,
				Data:  pktdata,
				AAC:   r.aacdecodeConfig.AAC,
			}

			r.muxer.WritePacket(avpkt)
		}
	}
}

func (r *Recorder) PushVideo(pkt *rtp.Packet) {

	r.videojitter.Add(pkt)
	pkts := r.videojitter.GetOrdered()

	if pkts != nil {
		for _, _pkt := range pkts {
			frame, _ := r.depacketizer.AddPacket(_pkt)
			if frame != nil {

				nalus, _ := h264.SplitNALUs(frame)

				nalus_ := make([][]byte, 0)
				keyframe := false

				for _, nalu := range nalus {

					switch h264.NALUType(nalu) {
					case h264.NALU_SPS:
						r.h264decodeConfig.H264.AddSPSPPS(nalu)
					case h264.NALU_PPS:
						r.h264decodeConfig.H264.AddSPSPPS(nalu)
						r.h264decodeConfig.Data = make([]byte, 5000)
						var len int
						r.h264decodeConfig.H264.ToConfig(r.h264decodeConfig.Data, &len)
						r.h264decodeConfig.Data = r.h264decodeConfig.Data[0:len]
					case h264.NALU_IDR:
						r.muxer.WritePacket(r.h264decodeConfig)
						nalus_ = append(nalus_, nalu)
						keyframe = true
					case h264.NALU_NONIDR:
						nalus_ = append(nalus_, nalu)
						keyframe = false
					case h264.NALU_SEI:
						continue
					}
				}

				if len(nalus_) == 0 {
					continue
				}

				duration := time.Since(r.startTime)
				data := h264.FillNALUsAVCC(nalus_)
				avpkt := av.Packet{
					Type:       av.H264,
					IsKeyFrame: keyframe,
					Time:       duration,
					CTime:      duration,
					Data:       data,
				}

				r.muxer.WritePacket(avpkt)
			}
		}
	}

}

func (r *Recorder) Close() {
	if r.flvfile != nil {
		r.flvfile.Close()
	}
}

func publishStream(c *gin.Context) {

	var data struct {
		Sdp string `json:"sdp"`
	}

	if err := c.ShouldBind(&data); err != nil {
		c.JSON(200, gin.H{"s": 10001, "e": err})
		return
	}

	// Create a MediaEngine object to configure the supported codec
	m := &webrtc.MediaEngine{}
	// Setup the codecs you want to use.
	// We'll use a H264 and Opus but you can also define your own
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	// Create a InterceptorRegistry. This is the user configurable RTP/RTCP Pipeline.
	// This provides NACKs, RTCP Reports and other features. If you use `webrtc.NewPeerConnection`
	// this is enabled by default. If you are manually managing You MUST create a InterceptorRegistry
	// for each PeerConnection.
	i := &interceptor.Registry{}

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	// Allow us to receive 1 audio track, and 1 video track
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	} else if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			fmt.Println("Ctrl+C the remote client to stop the demo")
		} else if connectionState == webrtc.ICEConnectionStateFailed {
			fmt.Println("Done writing media files")

			// Gracefully shutdown the peer connection
			if closeErr := peerConnection.Close(); closeErr != nil {
				panic(closeErr)
			}

			os.Exit(0)
		}
	})

	// Wait for the offer to be pasted
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  data.Sdp,
	}
	//signal.Decode(signal.MustReadStdin(), &offer)

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	recorder := newRecorder("record.flv")

	// Set a handler for when a new remote track starts, this handler saves buffers to disk as
	// an ivf file, since we could have multiple video tracks we provide a counter.
	// In your application this is where you would handle/process video
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		go func() {
			ticker := time.NewTicker(time.Second * 3)
			for range ticker.C {
				errSend := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
				if errSend != nil {
					fmt.Println(errSend)
				}
			}
		}()

		fmt.Printf("Track has started, of type %d: %s \n", track.PayloadType(), track.Codec().MimeType)

		//first := true

		for {
			rtpPacket, _, err := track.ReadRTP()
			if err != nil {
				if err == io.EOF {
					return
				}
				panic(err)
			}
			switch track.Kind() {
			case webrtc.RTPCodecTypeAudio:
				//recorder.PushAudio(rtpPacket)
				if len(rtpPacket.Payload) != 0 {
					recorder.PushAudio(rtpPacket)
				}
			case webrtc.RTPCodecTypeVideo:
				/*
					if first {
						_rtp := track.FirstRtpPacket()
						recorder.PushVideo(_rtp)
						first = false
					}
				*/
				recorder.PushVideo(rtpPacket)
				//fmt.Printf("Track receive video rtp : %s \n", rtpPacket.String())
			}
		}
	})

	c.JSON(200, gin.H{
		"s": 10000,
		"d": map[string]string{
			"sdp": peerConnection.LocalDescription().SDP,
		},
	})
}

func main() {

	router := gin.Default()
	corsc := cors.DefaultConfig()
	corsc.AllowAllOrigins = true
	corsc.AllowCredentials = true
	router.Use(cors.New(corsc))

	router.LoadHTMLFiles("./static/index.html")

	router.GET("/test", test)

	router.GET("/", index)

	router.POST("/rtc/v1/publish", publishStream)

	router.Run(":8080")
}
