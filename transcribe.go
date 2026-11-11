package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The longest stretch of audio transcribe hands the model: 20 minutes, 38 MB
// of 16 kHz mono PCM. A longer voice note is cut there and says so.
const maxAudioSeconds = 20 * 60

// Speech-to-text samples at 16 kHz, mono, 16-bit.
const sampleRate = 16000

// The longest transcript returned, in runes: 20 minutes of speech is well
// under it.
const maxTranscript = 60000

// The ffmpeg used when FFMPEG is unset; the nix build sets it to its own.
var defaultFFmpeg = "ffmpeg"

// transcriber turns an attachment's audio into text: ffmpeg decodes whatever
// the bridge stored (WhatsApp's Ogg/Opus voice notes, MP4 video notes, M4A…)
// into 16 kHz mono PCM, and an OpenAI-compatible /audio/transcriptions
// endpoint reads it. Its one destination is that endpoint: the server still
// fetches no URL a message names.
type transcriber struct {
	url   string // base URL, e.g. http://litellm:4000/v1
	key   string // bearer for that endpoint; may be empty
	model string // the model (or alias) to ask for
	http  *http.Client
	// decode turns a media file into raw 16 kHz mono s16le PCM, at most
	// maxAudioSeconds of it. ffmpeg in production; a stand-in in the tests.
	decode func(ctx context.Context, media []byte) ([]byte, error)
}

func newTranscriber(base, key, model, ffmpeg string) *transcriber {
	if model == "" {
		model = "stt"
	}
	if ffmpeg == "" {
		ffmpeg = defaultFFmpeg
	}
	return &transcriber{
		url:   strings.TrimRight(base, "/"),
		key:   key,
		model: model,
		// A cold model load plus twenty minutes of audio.
		http:   &http.Client{Timeout: 5 * time.Minute},
		decode: ffmpegDecoder(ffmpeg),
	}
}

// ffmpegDecoder runs ffmpeg on a temporary copy of the file: MP4 and M4A keep
// their index at the end, which a pipe cannot seek to.
func ffmpegDecoder(bin string) func(context.Context, []byte) ([]byte, error) {
	return func(ctx context.Context, media []byte) ([]byte, error) {
		f, err := os.CreateTemp("", "media-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(media); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-nostdin", "-hide_banner", "-loglevel", "error",
			// Two threads: the container counts threads against its pid limit.
			"-threads", "2", "-i", f.Name(), "-vn", "-ac", "1", "-ar", fmt.Sprint(sampleRate),
			"-t", fmt.Sprint(maxAudioSeconds), "-f", "s16le", "pipe:1")
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			// ffmpeg's message names the file's format, never its contents.
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 300 {
				msg = msg[:300]
			}
			return nil, fmt.Errorf("could not decode the audio: %v %s", err, msg)
		}
		return out.Bytes(), nil
	}
}

// wavHeader is the 44-byte header of a WAV holding n bytes of 16 kHz mono
// s16le PCM.
func wavHeader(n int) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + n))
	b.WriteString("WAVEfmt ")
	w(uint32(16))             // fmt chunk size
	w(uint16(1))              // PCM
	w(uint16(1))              // mono
	w(uint32(sampleRate))     // sample rate
	w(uint32(sampleRate * 2)) // byte rate
	w(uint16(2))              // block align
	w(uint16(16))             // bits per sample
	b.WriteString("data")
	w(uint32(n))
	return b.Bytes()
}

type transcript struct {
	Text     string
	Language string
	Seconds  float64
}

// transcribe sends 16 kHz mono PCM to the endpoint. language is an ISO 639-1
// code to force, or "" to let the model detect it.
func (t *transcriber) transcribe(ctx context.Context, pcm []byte, language string) (*transcript, error) {
	// Streamed: twenty minutes of audio is not copied again into a body.
	body, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := mw.WriteField("model", t.model)
		if err == nil {
			err = mw.WriteField("response_format", "verbose_json")
		}
		if err == nil && language != "" {
			err = mw.WriteField("language", language)
		}
		var fw io.Writer
		if err == nil {
			fw, err = mw.CreateFormFile("file", "audio.wav")
		}
		if err == nil {
			_, err = fw.Write(wavHeader(len(pcm)))
		}
		if err == nil {
			_, err = fw.Write(pcm)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	defer body.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", t.url+"/audio/transcriptions", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if t.key != "" {
		req.Header.Set("Authorization", "Bearer "+t.key)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("the speech-to-text service did not answer: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the speech-to-text service answered HTTP %d", resp.StatusCode)
	}
	var r struct {
		Text     string   `json:"text"`
		Language string   `json:"language"`
		Duration *float64 `json:"duration"`
		Segments []struct {
			Text         string  `json:"text"`
			AvgLogprob   float64 `json:"avg_logprob"`
			NoSpeechProb float64 `json:"no_speech_prob"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, errors.New("the speech-to-text service answered something that is not a transcript")
	}
	text := r.Text
	if len(r.Segments) > 0 {
		// Whisper's own filter: a segment the model thinks is silence and is
		// unsure of is a hallucination ("Subtítulos realizados por…"), not
		// speech.
		parts := make([]string, 0, len(r.Segments))
		for _, s := range r.Segments {
			if s.NoSpeechProb > 0.6 && s.AvgLogprob < -1 {
				continue
			}
			parts = append(parts, s.Text)
		}
		text = strings.Join(parts, " ")
	}
	out := &transcript{
		// Whisper breaks lines mid-sentence; the text is one paragraph.
		Text:     clean(strings.Join(strings.Fields(text), " "), maxTranscript),
		Language: strings.ToLower(clean(r.Language, 40)),
		Seconds:  float64(len(pcm)) / (sampleRate * 2),
	}
	if r.Duration != nil && *r.Duration > 0 {
		out.Seconds = *r.Duration
	}
	return out, nil
}

// ── the tool ────────────────────────────────────────────────────────────

type transcribeIn struct {
	Chat     string `json:"chat" jsonschema:"a chat id from list_chats"`
	Message  string `json:"message" jsonschema:"the id of a voice note, audio or video message (read_chat marks it fetchable)"`
	Language string `json:"language,omitempty" jsonschema:"ISO 639-1 code (en, es…) to force; leave empty to detect it, which handles every language"`
}

func (s *server) transcribe(ctx context.Context, _ *mcp.CallToolRequest, in transcribeIn) (*mcp.CallToolResult, any, error) {
	done, stop := s.begin("transcribe", in.Chat)
	if stop != nil {
		return done(nil, stop)
	}
	lang := strings.ToLower(strings.TrimSpace(in.Language))
	if lang != "" && (len(lang) != 2 || strings.Trim(lang, "abcdefghijklmnopqrstuvwxyz") != "") {
		return done(nil, fmt.Errorf("language %q: give a two-letter code such as en or es, or leave it empty", in.Language))
	}
	ch, err := s.chats.get(ctx, in.Chat)
	if err != nil {
		return done(nil, err)
	}
	ev, m, err := s.messageAt(ctx, ch.ID, in.Message)
	if err != nil {
		return done(nil, err)
	}
	if ev.Type != "m.room.message" || (m.MsgType != "m.audio" && m.MsgType != "m.video") || m.URL == "" {
		return done(nil, errors.New("that message is not a voice note, audio or video"))
	}
	data, _, err := s.mx.media(ctx, m.URL, maxFileBytes)
	if err != nil {
		return done(nil, err)
	}
	pcm, err := s.stt.decode(ctx, data)
	if err != nil {
		return done(nil, err)
	}
	if len(pcm) < sampleRate/5 { // under 0.1 s
		return done(nil, errors.New("that message has no audio to transcribe"))
	}
	tr, err := s.stt.transcribe(ctx, pcm, lang)
	if err != nil {
		return done(nil, err)
	}
	out := map[string]any{
		"note":       untrusted + " A transcript is also machine output: it can mishear names, numbers and mixed-language speech.",
		"chat":       ch.Name,
		"at":         when(ev.TS),
		"from":       s.chats.who(ev.Sender, nil),
		"kind":       strings.TrimPrefix(m.MsgType, "m."),
		"seconds":    int(tr.Seconds + 0.5),
		"transcript": tr.Text,
	}
	if tr.Language != "" {
		out["language"] = tr.Language
	}
	if len(pcm) >= maxAudioSeconds*sampleRate*2 {
		out["truncated"] = fmt.Sprintf("only the first %d minutes were transcribed", maxAudioSeconds/60)
	}
	if tr.Text == "" {
		out["transcript"] = ""
		out["empty"] = "no speech was heard"
	}
	if c := clean(caption(m), maxText); c != "" {
		out["caption"] = c
	}
	return done(answer(out), nil)
}
