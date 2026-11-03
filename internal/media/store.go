package media

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
)

// TypeCreativeDrafts holds generated posters (and later video finals) under
// review: public so the owner and Meta or WhatsApp can open the link, and
// cached briefly because a draft is replaced or deleted after review.
const TypeCreativeDrafts = "creative-drafts"

// TypeAudioDrafts holds voice-overs made for a draft: public because the
// presenter route fetches the audio by URL, purged with the draft.
const TypeAudioDrafts = "audio-drafts"

func init() {
	objectstore.Register(objectstore.MediaType{
		Name: TypeCreativeDrafts, Public: true, MaxBytes: 15 << 20,
		Allowed: []string{"image/jpeg", "image/png"}, CacheControl: "public, max-age=300",
	})
	objectstore.Register(objectstore.MediaType{
		Name: TypeAudioDrafts, Public: true, MaxBytes: 32 << 20,
		Allowed: []string{"audio/mpeg"}, CacheControl: "public, max-age=300",
	})
}

// PutAudio implements AudioStore: marketflow/<tenant>/audio-drafts/voice/<version>-<name>.mp3.
func (o ObjectStore) PutAudio(ctx context.Context, tenantID uuid.UUID, versionID, name string, data []byte) (string, string, error) {
	if o.S == nil || !o.S.Enabled() {
		return "", "", fmt.Errorf("media: object storage is not configured")
	}
	key, err := o.S.Key(objectstore.Owner{TenantID: tenantID.String()}, TypeAudioDrafts, "voice", versionID+"-"+name+".mp3")
	if err != nil {
		return "", "", err
	}
	if err := o.S.Put(ctx, key, data, "audio/mpeg"); err != nil {
		return "", "", err
	}
	return key, o.S.URL(key), nil
}

// ObjectStore stores posters with the shared httpware/objectstore library
// (Cloudflare R2 in production, MEDIA_ROOT locally).
type ObjectStore struct{ S *objectstore.Store }

// PutPoster implements Store: marketflow/<tenant>/creative-drafts/posters/<version>.jpg.
// The version id names the file, so a re-render of the same version replaces it.
func (o ObjectStore) PutPoster(ctx context.Context, tenantID uuid.UUID, versionID string, data []byte) (string, string, error) {
	if o.S == nil || !o.S.Enabled() {
		return "", "", fmt.Errorf("media: object storage is not configured")
	}
	key, err := o.S.Key(objectstore.Owner{TenantID: tenantID.String()}, TypeCreativeDrafts, "posters", versionID+".jpg")
	if err != nil {
		return "", "", err
	}
	if err := o.S.Put(ctx, key, data, "image/jpeg"); err != nil {
		return "", "", err
	}
	return key, o.S.URL(key), nil
}

// PutClip implements VideoStore: marketflow/<tenant>/video-drafts/clips/<version>.mp4
// (httpware's video-drafts type: public, short cache, mp4 only), streamed from disk.
func (o ObjectStore) PutClip(ctx context.Context, tenantID uuid.UUID, versionID, path string) (string, string, int64, error) {
	return o.putVideo(ctx, tenantID, "clips", versionID, path)
}

// PutAd implements AdStore: marketflow/<tenant>/video-drafts/ads/<version>.mp4.
func (o ObjectStore) PutAd(ctx context.Context, tenantID uuid.UUID, versionID, path string) (string, string, int64, error) {
	return o.putVideo(ctx, tenantID, "ads", versionID, path)
}

func (o ObjectStore) putVideo(ctx context.Context, tenantID uuid.UUID, folder, versionID, path string) (string, string, int64, error) {
	if o.S == nil || !o.S.Enabled() {
		return "", "", 0, fmt.Errorf("media: object storage is not configured")
	}
	key, err := o.S.Key(objectstore.Owner{TenantID: tenantID.String()}, objectstore.TypeVideoDrafts, folder, versionID+".mp4")
	if err != nil {
		return "", "", 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", 0, err
	}
	if err := o.S.PutStream(ctx, key, f, st.Size(), "video/mp4"); err != nil {
		return "", "", 0, err
	}
	return key, o.S.URL(key), st.Size(), nil
}

// SceneFiles is what the scene cache needs from storage: a scene clip of its own, and
// for a scene reused from an earlier version, a check that its file still exists and a
// copy under the new version's name, so purging the old version never takes the new
// version's files with it.
type SceneFiles interface {
	VideoStore
	Exists(ctx context.Context, key string) bool
	Copy(ctx context.Context, tenantID uuid.UUID, key, name string) (newKey, url string, err error)
}

// Exists implements SceneFiles; a lookup failure counts as missing (the scene is made again).
func (o ObjectStore) Exists(ctx context.Context, key string) bool {
	if o.S == nil || !o.S.Enabled() || key == "" {
		return false
	}
	ok, err := o.S.Exists(ctx, key)
	return err == nil && ok
}

// Copy implements SceneFiles: the file at key, which must be this tenant's, streamed to a
// sibling key named name (same media type and folder, same extension).
func (o ObjectStore) Copy(ctx context.Context, tenantID uuid.UUID, key, name string) (string, string, error) {
	if o.S == nil || !o.S.Enabled() {
		return "", "", fmt.Errorf("media: object storage is not configured")
	}
	if !strings.Contains(key, "/"+tenantID.String()+"/") || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("media: scene file is not this tenant's")
	}
	dst := path.Dir(key) + "/" + name + path.Ext(key)
	if dst == key {
		return key, o.S.URL(key), nil
	}
	r, size, ctype, err := o.S.Open(ctx, key)
	if err != nil {
		return "", "", err
	}
	defer r.Close()
	if ctype == "" {
		ctype = map[string]string{".mp4": "video/mp4", ".mp3": "audio/mpeg", ".jpg": "image/jpeg"}[path.Ext(key)]
	}
	if size < 0 {
		data, err := io.ReadAll(io.LimitReader(r, maxSceneBytes))
		if err != nil {
			return "", "", err
		}
		if err := o.S.Put(ctx, dst, data, ctype); err != nil {
			return "", "", err
		}
	} else if err := o.S.PutStream(ctx, dst, r, size, ctype); err != nil {
		return "", "", err
	}
	return dst, o.S.URL(dst), nil
}
