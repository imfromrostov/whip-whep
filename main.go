package main

import (
    "fmt"
    "io/ioutil" // Use this for reading request body
    "net/http"
    "sync"

    "github.com/pion/interceptor"
    "github.com/pion/interceptor/pkg/intervalpli"
    "github.com/pion/webrtc/v4"
)

var (
    peerConnectionConfiguration = webrtc.Configuration{
        ICEServers: []webrtc.ICEServer{
            {
                URLs: []string{"stun:stun.l.google.com:19302"},
            },
        },
    }
)

type Stream struct {
    videoTrack    *webrtc.TrackLocalStaticRTP
    peerConnection *webrtc.PeerConnection
}

var streams = make(map[string]*Stream)
var mu sync.Mutex

func main() {
    http.Handle("/", http.FileServer(http.Dir(".")))
    http.HandleFunc("/whep/", whepHandler)
    http.HandleFunc("/whip/", whipHandler)

    fmt.Println("Open http://localhost:8080/publish.html to publish stream")
    fmt.Println("Open http://localhost:8080/subscribe.html to subscribe to stream")
    panic(http.ListenAndServe(":8080", nil))
}

func whipHandler(w http.ResponseWriter, r *http.Request) {
    streamID := r.URL.Path[len("/whip/"):]

    mu.Lock()
    defer mu.Unlock()

    if _, exists := streams[streamID]; exists {
        http.Error(w, "Stream already exists", http.StatusConflict)
        return
    }

    var err error
    videoTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", streamID)
    if err != nil {
        http.Error(w, fmt.Sprintf("Failed to create video track: %v", err), http.StatusInternalServerError)
        return
    }
    
    fmt.Println("Created video track for stream ID:", streamID)

    m := &webrtc.MediaEngine{}
    if err = m.RegisterCodec(webrtc.RTPCodecParameters{
        RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, Channels: 0},
        PayloadType:        96,
    }, webrtc.RTPCodecTypeVideo); err != nil {
        http.Error(w, fmt.Sprintf("Failed to register codec: %v", err), http.StatusInternalServerError)
        return
    }

    i := &interceptor.Registry{}
    intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
    if err != nil {
        http.Error(w, fmt.Sprintf("Failed to create interceptor: %v", err), http.StatusInternalServerError)
        return
    }
    i.Add(intervalPliFactory)
    
    if err = webrtc.RegisterDefaultInterceptors(m, i); err != nil {
        http.Error(w, fmt.Sprintf("Failed to register default interceptors: %v", err), http.StatusInternalServerError)
        return
    }

    api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))
    peerConnection, err := api.NewPeerConnection(peerConnectionConfiguration)
    if err != nil {
        http.Error(w, fmt.Sprintf("Failed to create peer connection: %v", err), http.StatusInternalServerError)
        return
    }

    streams[streamID] = &Stream{videoTrack: videoTrack, peerConnection: peerConnection}

    peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
        for {
            pkt, _, err := track.ReadRTP()
            if err != nil {
                fmt.Println("Error reading RTP packet:", err)
                return
            }
            if err = videoTrack.WriteRTP(pkt); err != nil {
                fmt.Println("Error writing RTP packet:", err)
                return
            }
        }
    })

    offer, err := ioutil.ReadAll(r.Body) // Read the request body
    if err != nil {
        http.Error(w, "Failed to read request body", http.StatusInternalServerError)
        return
    }
    writeAnswer(w, peerConnection, offer, "/whip/"+streamID)
}

func whepHandler(w http.ResponseWriter, r *http.Request) {
    streamID := r.URL.Path[len("/whep/"):]

    mu.Lock()
    stream, exists := streams[streamID]
    mu.Unlock()

    if !exists {
        http.Error(w, "Stream not found", http.StatusNotFound)
        return
    }

    peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfiguration)
    if err != nil {
        http.Error(w, fmt.Sprintf("Failed to create peer connection: %v", err), http.StatusInternalServerError)
        return
    }

    rtpSender, err := peerConnection.AddTrack(stream.videoTrack)
    if err != nil {
        http.Error(w, fmt.Sprintf("Failed to add video track: %v", err), http.StatusInternalServerError)
        return
    }

    go func() {
        rtcpBuf := make([]byte, 1500)
        for {
            if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
                fmt.Println("Error reading RTCP packet:", rtcpErr)
                return
            }
        }
    }()

    offer, err := ioutil.ReadAll(r.Body) // Read the request body
    if err != nil {
        http.Error(w, "Failed to read request body", http.StatusInternalServerError)
        return
    }
    writeAnswer(w, peerConnection, offer, "/whep/"+streamID)
}

func writeAnswer(w http.ResponseWriter, peerConnection *webrtc.PeerConnection, offer []byte, path string) {
    if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
        http.Error(w, fmt.Sprintf("Failed to set remote description: %v", err), http.StatusInternalServerError)
        return
    }

    gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
    answer, err := peerConnection.CreateAnswer(nil)
    if err != nil {
        http.Error(w, fmt.Sprintf("Failed to create answer: %v", err), http.StatusInternalServerError)
        return
    } else if err = peerConnection.SetLocalDescription(answer); err != nil {
        http.Error(w, fmt.Sprintf("Failed to set local description: %v", err), http.StatusInternalServerError)
        return
    }

    <-gatherComplete
    w.Header().Add("Location", path)
    w.WriteHeader(http.StatusCreated)
    fmt.Fprint(w, peerConnection.LocalDescription().SDP)
}
