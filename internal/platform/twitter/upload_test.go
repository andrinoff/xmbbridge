package twitter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"
)

// fakeUpload emulates the X media upload endpoints and records the calls.
type fakeUpload struct {
	mu sync.Mutex

	commands []string // command sequence, e.g. INIT, APPEND, FINALIZE, STATUS
	segments []string // contents of each APPEND segment
	simple   []string // contents of each simple upload

	altTexts map[string]string

	// processing makes FINALIZE report a pending state once, which forces the
	// STATUS polling path to be exercised.
	processing bool
	finalized  bool

	nextID      int
	lastMediaID string
	totalBytes  string
	mediaType   string
	category    string
}

func (f *fakeUpload) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.altTexts == nil {
			f.altTexts = map[string]string{}
		}

		switch r.URL.Path {
		case "/1.1/media/upload.json":
			// APPEND carries its command on the query string; INIT, FINALIZE and
			// STATUS send it in the form body; a simple upload has no command at
			// all and is recognised by its multipart body.
			command := r.URL.Query().Get("command")
			isMultipart := strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data")
			if command == "" && !isMultipart {
				if err := r.ParseForm(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				command = r.Form.Get("command")
			}

			if command == "" {
				f.simple = append(f.simple, readMultipartFile(r, "media"))
				f.nextID++
				id := strconv.Itoa(1000 + f.nextID)
				writeJSON(w, http.StatusOK, map[string]any{"media_id": 1000 + f.nextID, "media_id_string": id})
				return
			}

			if command == "APPEND" {
				f.commands = append(f.commands, "APPEND")
				f.segments = append(f.segments, readMultipartFile(r, "media"))
				w.WriteHeader(http.StatusNoContent)
				return
			}

			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			form := map[string]string{}
			for key, values := range r.Form {
				form[key] = values[0]
			}

			switch command {
			case "INIT":
				f.commands = append(f.commands, "INIT")
				f.nextID++
				f.lastMediaID = strconv.Itoa(2000 + f.nextID)
				f.totalBytes = form["total_bytes"]
				f.mediaType = form["media_type"]
				f.category = form["media_category"]
				writeJSON(w, http.StatusOK, map[string]any{"media_id": 2000 + f.nextID, "media_id_string": f.lastMediaID})
			case "FINALIZE":
				f.commands = append(f.commands, "FINALIZE")
				id := form["media_id"]
				// The real API returns media_id as a number and the string form
				// alongside it.
				response := map[string]any{"media_id": atoiOrDefault(id), "media_id_string": id}
				if f.processing && !f.finalized {
					f.finalized = true
					response["processing_info"] = map[string]any{
						"state": "pending", "check_after_secs": 1,
					}
				}
				writeJSON(w, http.StatusOK, response)
			case "STATUS":
				f.commands = append(f.commands, "STATUS")
				writeJSON(w, http.StatusOK, map[string]any{
					"media_id":        atoiOrDefault(form["media_id"]),
					"media_id_string": form["media_id"],
					"processing_info": map[string]any{"state": "succeeded"},
				})
			default:
				http.Error(w, "unknown command", http.StatusBadRequest)
			}

		case "/1.1/media/metadata/create.json":
			var payload struct {
				MediaID string `json:"media_id"`
				AltText struct {
					Text string `json:"text"`
				} `json:"alt_text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.altTexts[payload.MediaID] = payload.AltText.Text
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	})
}

func readMultipartFile(r *http.Request, field string) string {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return ""
	}
	file, _, err := r.FormFile(field)
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return ""
	}
	return string(data)
}

// atoiOrDefault converts a media id the fakes hold as a string back to the
// numeric form the real API returns.
func atoiOrDefault(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

// newPostingAdapter wires an adapter to a fake media upload host and a fake
// create-tweet host, both with OAuth 1.0a credentials so posting is enabled.
func newPostingAdapter(t *testing.T, upload *fakeUpload, createTweet func(w http.ResponseWriter, r *http.Request)) (*Adapter, *fakeUpload) {
	t.Helper()

	uploadServer := httptest.NewServer(upload.handler())
	t.Cleanup(uploadServer.Close)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2/tweets" {
			// The request body is left untouched for the test to consume.
			createTweet(w, r)
			return
		}
		http.Error(w, "unexpected path", http.StatusNotFound)
	}))
	t.Cleanup(apiServer.Close)

	adapter, err := New(Options{
		Config:     oauth1Config(),
		State:      testStore(t),
		Logger:     discardLogger(),
		APIHost:    apiServer.URL,
		UploadHost: uploadServer.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !adapter.CanPost() {
		t.Fatal("OAuth 1.0a credentials should make the adapter writable")
	}
	return adapter, upload
}

func TestPostPublishesTextAndImages(t *testing.T) {
	var created []byte
	upload := &fakeUpload{}
	adapter, upload := newPostingAdapter(t, upload, func(w http.ResponseWriter, r *http.Request) {
		created, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]string{"id": "777", "text": "bridged"}})
	})

	id, err := adapter.Post(context.Background(), model.Outbound{
		Origin:   model.PlatformMastodon,
		OriginID: "1",
		Text:     "hello from mastodon",
	}, []model.PreparedMedia{
		{Kind: model.MediaImage, Bytes: []byte("fake-jpeg-bytes"), MimeType: "image/jpeg", AltText: "a cat", Filename: "a.jpg"},
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if id != "777" {
		t.Fatalf("id = %q", id)
	}

	// The image went up through the simple upload and its alt text with it.
	if len(upload.simple) != 1 || upload.simple[0] != "fake-jpeg-bytes" {
		t.Fatalf("simple uploads = %v", upload.simple)
	}
	if len(upload.altTexts) != 1 {
		t.Fatalf("alt texts = %v", upload.altTexts)
	}
	for _, alt := range upload.altTexts {
		if alt != "a cat" {
			t.Fatalf("alt text = %q", alt)
		}
	}

	// The tweet creation carries both the text and the media id.
	var payload struct {
		Text  string `json:"text"`
		Media *struct {
			IDs []string `json:"media_ids"`
		} `json:"media"`
	}
	if err := json.Unmarshal(created, &payload); err != nil {
		t.Fatalf("decode create request: %v", err)
	}
	if payload.Text != "hello from mastodon" {
		t.Fatalf("text = %q", payload.Text)
	}
	if payload.Media == nil || len(payload.Media.IDs) != 1 {
		t.Fatalf("media ids = %+v", payload.Media)
	}
}

func TestPostUploadsVideoInChunksAndWaitsForProcessing(t *testing.T) {
	upload := &fakeUpload{processing: true}
	adapter, upload := newPostingAdapter(t, upload, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]string{"id": "888", "text": "clip"}})
	})

	// Larger than one segment so the chunked path is genuinely exercised.
	video := strings.Repeat("v", 2*uploadChunkSize+100)
	if _, err := adapter.Post(context.Background(), model.Outbound{
		Origin:   model.PlatformBluesky,
		OriginID: "at://did/post/9",
		Text:     "a clip",
	}, []model.PreparedMedia{
		{Kind: model.MediaVideo, Bytes: []byte(video), MimeType: "video/mp4", Filename: "clip.mp4"},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	// INIT then three APPEND rounds then FINALIZE and STATUS.
	if len(upload.commands) < 6 {
		t.Fatalf("command sequence too short: %v", upload.commands)
	}
	if upload.commands[0] != "INIT" {
		t.Fatalf("first command = %q, want INIT", upload.commands[0])
	}
	appends := 0
	for _, command := range upload.commands {
		if command == "APPEND" {
			appends++
		}
	}
	if appends != 3 {
		t.Fatalf("APPEND ran %d times, want 3 for %d bytes", appends, len(video))
	}
	if upload.commands[len(upload.commands)-1] != "STATUS" {
		t.Fatalf("last command = %q, want STATUS", upload.commands[len(upload.commands)-1])
	}

	// The segments reassemble into exactly the original video.
	var reassembled strings.Builder
	for _, segment := range upload.segments {
		reassembled.WriteString(segment)
	}
	if reassembled.String() != video {
		t.Fatalf("segments do not reassemble the original (%d of %d bytes)", reassembled.Len(), len(video))
	}

	// The INIT metadata describes what the API needs for scheduling.
	if upload.totalBytes != strconv.Itoa(len(video)) {
		t.Fatalf("total_bytes = %q", upload.totalBytes)
	}
	if upload.mediaType != "video/mp4" {
		t.Fatalf("media_type = %q", upload.mediaType)
	}
	if upload.category != "tweet_video" {
		t.Fatalf("media_category = %q", upload.category)
	}
}

func TestPostUploadsGIFsAsGIFCategory(t *testing.T) {
	upload := &fakeUpload{}
	adapter, upload := newPostingAdapter(t, upload, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]string{"id": "889", "text": "gif"}})
	})

	if _, err := adapter.Post(context.Background(), model.Outbound{
		Origin:   model.PlatformMastodon,
		OriginID: "2",
		Text:     "loop",
	}, []model.PreparedMedia{
		{Kind: model.MediaGIF, Bytes: []byte("gif-bytes"), MimeType: "video/mp4", Filename: "loop.mp4"},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if upload.category != "tweet_gif" {
		t.Fatalf("media_category = %q, want tweet_gif", upload.category)
	}
}

func TestPostThreadsRepliesOntoTheBridgedParent(t *testing.T) {
	var created []byte
	upload := &fakeUpload{}
	adapter, upload := newPostingAdapter(t, upload, func(w http.ResponseWriter, r *http.Request) {
		created, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]string{"id": "890", "text": "reply"}})
	})

	if _, err := adapter.Post(context.Background(), model.Outbound{
		Origin:         model.PlatformBluesky,
		OriginID:       "at://did/post/10",
		Text:           "a reply",
		IsReply:        true,
		ParentTargetID: "123456",
	}, nil); err != nil {
		t.Fatalf("Post: %v", err)
	}

	var payload struct {
		Reply *struct {
			InReplyToTweetID string `json:"in_reply_to_tweet_id"`
		} `json:"reply"`
	}
	if err := json.Unmarshal(created, &payload); err != nil {
		t.Fatalf("decode create request: %v", err)
	}
	if payload.Reply == nil || payload.Reply.InReplyToTweetID != "123456" {
		t.Fatalf("reply = %+v, want the bridged parent", payload.Reply)
	}
}

func TestPostCapsAttachmentsAtFour(t *testing.T) {
	upload := &fakeUpload{}
	adapter, upload := newPostingAdapter(t, upload, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]string{"id": "891", "text": "many"}})
	})

	media := make([]model.PreparedMedia, 6)
	for i := range media {
		media[i] = model.PreparedMedia{
			Kind:     model.MediaImage,
			Bytes:    []byte("img"),
			MimeType: "image/jpeg",
			Filename: strconv.Itoa(i),
		}
	}

	if _, err := adapter.Post(context.Background(), model.Outbound{
		Origin:   model.PlatformMastodon,
		OriginID: "3",
		Text:     "many images",
	}, media); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(upload.simple) != maxMediaPerTweet {
		t.Fatalf("uploaded %d images, want the cap of %d", len(upload.simple), maxMediaPerTweet)
	}
}

func TestPostRejectsMediaWithoutUploadCredentials(t *testing.T) {
	// Bearer-only adapters are read-only; posting must fail clearly rather
	// than fire requests that the API would reject.
	adapter, err := New(Options{
		Config: bearerTestConfig(),
		State:  testStore(t),
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if adapter.CanPost() {
		t.Fatal("a bearer-only adapter cannot post")
	}

	_, err = adapter.Post(context.Background(), model.Outbound{
		Origin:   model.PlatformMastodon,
		OriginID: "4",
		Text:     "should fail",
	}, []model.PreparedMedia{{Kind: model.MediaImage, Bytes: []byte("x"), MimeType: "image/jpeg"}})
	if err == nil {
		t.Fatal("expected an error posting without OAuth 1.0a credentials")
	}
	if !strings.Contains(err.Error(), "OAuth 1.0a") {
		t.Fatalf("error should name the missing credential type: %v", err)
	}
}

func TestPostSurfacesCreditExhaustion(t *testing.T) {
	upload := &fakeUpload{}
	adapter, _ := newPostingAdapter(t, upload, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"detail":"credits depleted","status":402,`+
			`"title":"Payment Required","type":"https://api.x.com/2/problems/credits-depleted"}`)
	})

	_, err := adapter.Post(context.Background(), model.Outbound{
		Origin:   model.PlatformMastodon,
		OriginID: "5",
		Text:     "should fail",
	}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "create tweet") {
		t.Fatalf("error should be wrapped with context: %v", err)
	}
}

func TestUploadRejectsAPIFailures(t *testing.T) {
	// Point a dedicated adapter at an upload host that always fails. The
	// upload is attempted before the post, so no create-tweet call is made.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()

	failingAdapter, err := New(Options{
		Config:     oauth1Config(),
		State:      testStore(t),
		Logger:     discardLogger(),
		UploadHost: failing.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = failingAdapter.Post(context.Background(), model.Outbound{
		Origin:   model.PlatformMastodon,
		OriginID: "6",
		Text:     "should fail",
	}, []model.PreparedMedia{{Kind: model.MediaImage, Bytes: []byte("x"), MimeType: "image/jpeg"}})
	if err == nil {
		t.Fatal("expected an error when the upload host fails")
	}
	if !strings.Contains(err.Error(), "media upload rejected") {
		t.Fatalf("error should mention the rejected upload: %v", err)
	}
}

func TestLimitsReflectTheXAPIDefaults(t *testing.T) {
	cfg := oauth1Config()
	// The defaults are normally applied by config.Load; set them here since the
	// adapter is built directly.
	cfg.PlatformLimits = config.PlatformLimits{
		MaxImageBytes:    5 << 20,
		MaxVideoBytes:    512 << 20,
		MaxImageDim:      4096,
		MaxVideoDuration: config.Duration(140 * time.Second),
		Video:            true,
	}
	adapter, err := New(Options{Config: cfg, State: testStore(t), Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	limits := adapter.Limits()
	if limits.TextMax != 280 {
		t.Fatalf("text max = %d", limits.TextMax)
	}
	if limits.ImageMaxBytes != 5<<20 {
		t.Fatalf("image bytes = %d", limits.ImageMaxBytes)
	}
	if limits.VideoMaxDuration != 140*time.Second {
		t.Fatalf("video duration = %v", limits.VideoMaxDuration)
	}
}
