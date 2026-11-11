package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A speech-to-text stand-in: it checks the request is the one an
// OpenAI-compatible /audio/transcriptions takes and answers verbose_json,
// one segment of it the silence hallucination Whisper is known for.
type fakeSTT struct {
	t     *testing.T
	mu    sync.Mutex
	calls int
	lang  string
}

func (f *fakeSTT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if r.URL.Path != "/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer sk-test" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		f.t.Errorf("not multipart: %v", err)
	}
	if got := r.FormValue("model"); got != "stt" {
		f.t.Errorf("model = %q, want stt", got)
	}
	if got := r.FormValue("response_format"); got != "verbose_json" {
		f.t.Errorf("response_format = %q", got)
	}
	f.mu.Lock()
	f.lang = r.FormValue("language")
	f.mu.Unlock()
	file, _, err := r.FormFile("file")
	if err != nil {
		f.t.Fatalf("no file: %v", err)
	}
	b, _ := io.ReadAll(file)
	if !bytes.HasPrefix(b, []byte("RIFF")) || string(b[8:16]) != "WAVEfmt " || binary.LittleEndian.Uint32(b[24:28]) != sampleRate || int(binary.LittleEndian.Uint32(b[40:44])) != len(b)-44 {
		f.t.Errorf("not a 16 kHz WAV: % x", b[:44])
	}
	writeJSON(w, map[string]any{
		"text":     "ignored when segments are present",
		"language": "spanish",
		"duration": 5.7,
		"segments": []any{
			map[string]any{"text": " Hola, te mando este audio para avisarte que mañana llego\n", "avg_logprob": -0.02, "no_speech_prob": 0.0},
			map[string]any{"text": " tarde a la reunión.", "avg_logprob": -0.1, "no_speech_prob": 0.01},
			map[string]any{"text": " Subtítulos realizados por la comunidad de Amara.org", "avg_logprob": -1.4, "no_speech_prob": 0.9},
		},
	})
}

// voiceHS is the homeserver stand-in plus a voice note, a video note and the
// media behind them.
type voiceHS struct{ *fakeHS }

func (v voiceHS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/_matrix/client/v3/rooms/" + dmRoom + "/event/$voice":
		if r.Header.Get("Authorization") != "Bearer tok" {
			break
		}
		writeJSON(w, ev("m.room.message", "$voice", ghostBob, 1007, map[string]any{"msgtype": "m.audio", "body": "Voice message", "url": "mxc://example.org/voice", "info": map[string]any{"mimetype": "audio/ogg", "duration": 5700}, "org.matrix.msc3245.voice": map[string]any{}}))
		return
	case "/_matrix/client/v3/rooms/" + dmRoom + "/event/$late":
		// The bridge's first copy, before it had the file.
		writeJSON(w, ev("m.room.message", "$late", ghostBob, 1008, map[string]any{"msgtype": "m.text", "body": "Voice message"}))
		return
	case "/_matrix/client/v1/rooms/" + dmRoom + "/relations/$late/m.replace":
		edit := func(id, sender, url string) map[string]any {
			return ev("m.room.message", id, sender, 1009, map[string]any{"msgtype": "m.audio", "body": "* Voice message", "url": "mxc://example.org/x",
				"m.new_content": map[string]any{"msgtype": "m.audio", "body": "Voice message", "url": url},
				"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": "$late"}})
		}
		writeJSON(w, map[string]any{"chunk": []any{
			edit("$imposter", operator, "mxc://example.org/abc"), // not the sender's: ignored
			edit("$fix", ghostBob, "mxc://example.org/voice"),
		}})
		return
	case "/_matrix/client/v1/media/download/example.org/voice":
		if r.Header.Get("Authorization") != "Bearer tok" {
			break
		}
		w.Header().Set("Content-Type", "audio/ogg")
		_, _ = w.Write([]byte("OggS opus"))
		return
	}
	v.fakeHS.ServeHTTP(w, r)
}

func connectSTT(t *testing.T, decode func(context.Context, []byte) ([]byte, error)) (*mcp.ClientSession, *fakeSTT) {
	t.Helper()
	hs := voiceHS{&fakeHS{t: t}}
	srv := httptest.NewServer(hs)
	t.Cleanup(srv.Close)
	stt := &fakeSTT{t: t}
	sttSrv := httptest.NewServer(stt)
	t.Cleanup(sttSrv.Close)
	mx := newMatrixClient("", srv.URL, "reader", "pw")
	tr := newTranscriber(sttSrv.URL+"/v1/", "sk-test", "", "")
	tr.decode = decode
	s := &server{mx: mx, chats: &chats{mx: mx, operator: operator, network: "whatsapp"}, limit: &limiter{max: 100}, label: "WhatsApp", stt: tr}
	m := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	s.register(m)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := m.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, stt
}

// Five seconds of silence, as the decoder would hand it over.
func fakeDecode(_ context.Context, media []byte) ([]byte, error) {
	if string(media) != "OggS opus" {
		return nil, errors.New("decoded the wrong file")
	}
	return make([]byte, 5*sampleRate*2), nil
}

func TestTranscribe(t *testing.T) {
	cs, stt := connectSTT(t, fakeDecode)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tl := range tools.Tools {
		found = found || tl.Name == "transcribe"
	}
	if !found {
		t.Fatal("transcribe is not offered with an endpoint configured")
	}
	out, isErr := callJSON(t, cs, "transcribe", map[string]any{"chat": dmRoom, "message": "$voice"})
	if isErr {
		t.Fatal(out)
	}
	if got, want := out["transcript"], "Hola, te mando este audio para avisarte que mañana llego tarde a la reunión."; got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
	if out["language"] != "spanish" || out["seconds"] != float64(6) || out["kind"] != "audio" || out["from"] == nil {
		t.Errorf("answer = %v", out)
	}
	if !strings.Contains(out["note"].(string), "written by other people") {
		t.Errorf("no untrusted note: %v", out["note"])
	}
	if stt.lang != "" {
		t.Errorf("language sent unasked: %q", stt.lang)
	}
	if _, isErr := callJSON(t, cs, "transcribe", map[string]any{"chat": dmRoom, "message": "$voice", "language": "EN"}); isErr || stt.lang != "en" {
		t.Errorf("language not forced: %q", stt.lang)
	}
}

func TestTranscribeRefuses(t *testing.T) {
	cs, stt := connectSTT(t, fakeDecode)
	for _, c := range []map[string]any{
		{"chat": dmRoom, "message": "$m1"},                           // text
		{"chat": dmRoom, "message": "$img"},                          // a photo
		{"chat": botRoom, "message": "$voice"},                       // not a chat
		{"chat": dmRoom, "message": "$voice", "language": "spanish"}, // not a code
		{"chat": dmRoom, "message": "$voice", "language": "e1"},      // not a code
	} {
		if out, isErr := callJSON(t, cs, "transcribe", c); !isErr {
			t.Errorf("%v: answered %v", c, out)
		}
	}
	if stt.calls != 0 {
		t.Errorf("the endpoint was called %d times for refused requests", stt.calls)
	}
}

func TestTranscribeIsOffWithoutAnEndpoint(t *testing.T) {
	cs, _ := connect(t, nil, 100)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "transcribe" {
			t.Fatal("transcribe offered with no endpoint")
		}
	}
}

// A file ffmpeg cannot read is a tool error, and nothing reaches the endpoint.
func TestTranscribeDecodeFailure(t *testing.T) {
	cs2, stt := connectSTT(t, func(context.Context, []byte) ([]byte, error) { return nil, errors.New("could not decode the audio") })
	if out, isErr := callJSON(t, cs2, "transcribe", map[string]any{"chat": dmRoom, "message": "$voice"}); !isErr || !strings.Contains(out["error"].(string), "decode") {
		t.Errorf("decode failure: %v", out)
	}
	if stt.calls != 0 {
		t.Error("undecodable audio reached the endpoint")
	}
}

// The real decoder, when an ffmpeg is at hand (the nix build provides one):
// a WAV at 44.1 kHz stereo comes out as 16 kHz mono PCM of the same length.
func TestFFmpegDecoder(t *testing.T) {
	bin := defaultFFmpeg
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("no ffmpeg")
	}
	// One second of 44.1 kHz stereo silence.
	var in bytes.Buffer
	pcm := make([]byte, 44100*4)
	in.WriteString("RIFF")
	_ = binary.Write(&in, binary.LittleEndian, uint32(36+len(pcm)))
	in.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(2), uint32(44100), uint32(44100 * 4), uint16(4), uint16(16)} {
		_ = binary.Write(&in, binary.LittleEndian, v)
	}
	in.WriteString("data")
	_ = binary.Write(&in, binary.LittleEndian, uint32(len(pcm)))
	in.Write(pcm)
	out, err := ffmpegDecoder(bin)(context.Background(), in.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < sampleRate*2*9/10 || len(out) > sampleRate*2*11/10 {
		t.Errorf("decoded %d bytes, want about %d", len(out), sampleRate*2)
	}
	if _, err := ffmpegDecoder(bin)(context.Background(), []byte("not audio")); err == nil {
		t.Error("garbage decoded")
	}
}

// A voice note whose file arrived in an edit is transcribed from the edit,
// and only an edit by its own sender counts.
func TestTranscribeFollowsTheSendersEdit(t *testing.T) {
	cs, _ := connectSTT(t, fakeDecode) // fakeDecode refuses any file but the voice note
	if out, isErr := callJSON(t, cs, "transcribe", map[string]any{"chat": dmRoom, "message": "$late"}); isErr {
		t.Fatal(out)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_file", Arguments: map[string]any{"chat": dmRoom, "message": "$late"}})
	if err != nil || res.IsError {
		t.Fatalf("get_file on an edited voice note: %v %v", err, res.Content)
	}
	if _, ok := res.Content[1].(*mcp.AudioContent); !ok {
		t.Errorf("get_file returned %T, want audio", res.Content[1])
	}
}
