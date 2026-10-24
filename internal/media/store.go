package media

import (
	"context"
	"fmt"
	"os"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
)

// TypeCreativeDrafts holds generated posters (and later video finals) under
// review: public so the owner and Meta or WhatsApp can open the link, and
// cached briefly because a draft is replaced or deleted after review.
const TypeCreativeDrafts = "creative-drafts"

func init() {
	objectstore.Register(objectstore.MediaType{
		Name: TypeCreativeDrafts, Public: true, MaxBytes: 15 << 20,
		Allowed: []string{"image/jpeg", "image/png"}, CacheControl: "public, max-age=300",
	})
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
	if o.S == nil || !o.S.Enabled() {
		return "", "", 0, fmt.Errorf("media: object storage is not configured")
	}
	key, err := o.S.Key(objectstore.Owner{TenantID: tenantID.String()}, objectstore.TypeVideoDrafts, "clips", versionID+".mp4")
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
