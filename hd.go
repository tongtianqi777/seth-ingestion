package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"go.mau.fi/whatsmeow/proto/waE2E"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/tianqitong/seth-ingestion/db"
)

// WhatsApp sends a photo or video chosen as "HD quality" as two messages: a
// low-resolution one (paired media type SD_*_PARENT) and, a moment later, the
// HD one, whose message association names the low-resolution message as its
// parent. History sync may deliver them in either order. Only the HD file is
// kept, in the place of the low-resolution one: the same messages row and
// album position.

// hdParent returns the ID of the low-resolution message that msg is the HD copy of.
func hdParent(msg *waE2E.Message) (string, bool) {
	assoc := msg.GetMessageContextInfo().GetMessageAssociation()
	switch assoc.GetAssociationType() {
	case waE2E.MessageAssociation_HD_IMAGE_DUAL_UPLOAD, waE2E.MessageAssociation_HD_VIDEO_DUAL_UPLOAD:
		id := assoc.GetParentMessageKey().GetID()
		return id, id != ""
	}
	return "", false
}

// isLowResParent reports whether md is the low-resolution half of an HD pair.
func isLowResParent(md *media) bool {
	switch md.context.GetPairedMediaType() {
	case waE2E.ContextInfo_SD_IMAGE_PARENT, waE2E.ContextInfo_SD_VIDEO_PARENT:
		return true
	}
	return false
}

// mergeHD moves the file of the stored HD message hdID into the place of its
// low-resolution parent, deletes the HD message's own row and the
// low-resolution file. It does nothing, and returns false, if the parent has
// no stored place or the HD message has no saved file.
func mergeHD(ctx context.Context, store *db.Store, mediaDir string, log waLog.Logger, chatJID, hdID, parentID string) (bool, error) {
	hd, err := store.MediaFile(ctx, chatJID, hdID)
	if err != nil || hd == nil || hd.RelativePath == "" {
		return false, err
	}
	old, ok, err := store.ReplaceMediaFile(ctx, chatJID, parentID, *hd)
	if err != nil || !ok {
		return false, err
	}
	if err := store.DeleteMessage(ctx, chatJID, hdID); err != nil {
		return false, err
	}
	removeMedia(mediaDir, old, hd.RelativePath, log)
	log.Infof("Kept the HD file %s of message %s; dropped its low-resolution copy", hd.RelativePath, parentID)
	return true, nil
}

// mergeHDDuplicates merges every stored HD message into its low-resolution
// parent, which cleans up files saved by versions that kept both. It returns
// the number of pairs merged.
func mergeHDDuplicates(ctx context.Context, store *db.Store, mediaDir string, log waLog.Logger) (int, error) {
	children, err := store.HDChildren(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range children {
		merged, err := mergeHD(ctx, store, mediaDir, log, c.ChatJID, c.MessageID, c.ParentID)
		if err != nil {
			return n, err
		}
		if merged {
			n++
		}
	}
	return n, nil
}

// removeMedia deletes the file at rel, a path relative to the media dir,
// unless it is empty or is keep, the file that replaces it.
func removeMedia(mediaDir, rel, keep string, log waLog.Logger) {
	if rel == "" || rel == keep {
		return
	}
	if err := os.Remove(filepath.Join(mediaDir, filepath.FromSlash(rel))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warnf("Failed to delete %s: %v", rel, err)
	}
}
