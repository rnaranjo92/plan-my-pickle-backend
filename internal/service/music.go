package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Spotify jukebox: the per-event player-driven music queue. Playback + search
// (the Spotify-token parts) live in spotify.go; this file is the queue + the
// organizer's moderation, none of which touches Spotify directly.

// Validation errors the API maps to 400 (everything else is a 500).
var (
	ErrMusicDisabled = errors.New("music is not enabled for this event")
	ErrInvalidTrack  = errors.New("a valid Spotify track is required")
)

const musicQueueCols = "id,track_uri,track_name,artist,album_art,duration_ms," +
	"added_by_user_id,added_by_name,status,created_at"

func mapMusicTrack(m map[string]any, callerID string) model.MusicTrack {
	uid := asStr(m, "added_by_user_id")
	return model.MusicTrack{
		ID:          asStr(m, "id"),
		TrackURI:    asStr(m, "track_uri"),
		TrackName:   asStr(m, "track_name"),
		Artist:      asStr(m, "artist"),
		AlbumArt:    asStr(m, "album_art"),
		DurationMs:  asInt(m, "duration_ms"),
		AddedByName: asStr(m, "added_by_name"),
		Status:      asStr(m, "status"),
		CreatedAt:   asStr(m, "created_at"),
		IsMine:      callerID != "" && uid == callerID,
	}
}

// musicSettings reads an event's jukebox flags + owner in one shot.
func (s *Service) musicSettings(eventID string) (enabled, requireApproval bool, ownerID string, err error) {
	row, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=music_enabled,music_require_approval,owner_id")
	if err != nil {
		return false, false, "", err
	}
	if row == nil {
		return false, false, "", ErrNotFound
	}
	return row["music_enabled"] == true, row["music_require_approval"] == true,
		asStr(row, "owner_id"), nil
}

// AddToQueue adds a searched Spotify track to an event's queue. A player's add
// lands as 'pending' when the organizer requires approval (else 'queued', ready
// to play). The organizer's own adds skip approval. Returns the stored row.
// capRunes truncates s to at most n runes (multibyte-safe), for display strings
// that must not carry oversized/abusive text.
func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (s *Service) AddToQueue(eventID, userID, addedByName string, req model.AddTrackRequest) (model.MusicTrack, error) {
	var out model.MusicTrack
	uri := strings.TrimSpace(req.TrackURI)
	name := strings.TrimSpace(req.TrackName)
	if !strings.HasPrefix(uri, "spotify:track:") || name == "" {
		return out, ErrInvalidTrack
	}
	enabled, requireApproval, ownerID, err := s.musicSettings(eventID)
	if err != nil {
		return out, err
	}
	if !enabled {
		return out, ErrMusicDisabled
	}
	status := "queued"
	if requireApproval && userID != ownerID {
		status = "pending"
	}
	row := map[string]any{
		"event_id":    eventID,
		"track_uri":   uri,
		"track_name":  name,
		"artist":      strings.TrimSpace(req.Artist),
		"album_art":   strings.TrimSpace(req.AlbumArt),
		"duration_ms": req.DurationMs,
		"status":      status,
	}
	if userID != "" {
		row["added_by_user_id"] = userID
	}
	// Cap the caller-supplied display name (shown on the public queue + TV) so it
	// can't carry oversized/abusive text.
	if n := capRunes(strings.TrimSpace(addedByName), 40); n != "" {
		row["added_by_name"] = n
	}
	ins, err := s.sb.Insert("music_queue", row)
	if err != nil {
		return out, err
	}
	if len(ins) == 0 {
		return out, errors.New("queue insert returned no row")
	}
	return mapMusicTrack(ins[0], userID), nil
}

// MusicQueue returns an event's live queue (pending + queued + playing, oldest
// first) — the played/skipped history is dropped. callerID flags the caller's
// own tracks so a player can remove their request.
func (s *Service) MusicQueue(eventID, callerID string) ([]model.MusicTrack, error) {
	rows, err := s.sb.Select("music_queue",
		"event_id=eq."+store.Q(eventID)+
			"&status=in.(pending,queued,playing)"+
			"&order=created_at.asc&select="+musicQueueCols)
	if err != nil {
		return nil, err
	}
	out := make([]model.MusicTrack, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapMusicTrack(r, callerID))
	}
	return out, nil
}

// TrackEventID returns a queue row's event_id + added_by_user_id (for auth checks).
func (s *Service) TrackEventID(trackID string) (eventID, addedBy string, err error) {
	row, err := s.sb.SelectOne("music_queue",
		"id=eq."+store.Q(trackID)+"&select=event_id,added_by_user_id")
	if err != nil {
		return "", "", err
	}
	if row == nil {
		return "", "", ErrNotFound
	}
	return asStr(row, "event_id"), asStr(row, "added_by_user_id"), nil
}

// setTrackStatus flips one track's status (moderation + playback state).
func (s *Service) setTrackStatus(trackID, status string) error {
	_, err := s.sb.Update("music_queue", "id=eq."+store.Q(trackID),
		map[string]any{"status": status})
	return err
}

// ApproveTrack clears a pending track for play (organizer-only).
func (s *Service) ApproveTrack(trackID string) error {
	return s.setTrackStatus(trackID, "queued")
}

// SkipTrack drops a track from the queue (organizer, or the player who added
// it). It's marked skipped rather than deleted so it doesn't get re-served.
func (s *Service) SkipTrack(trackID string) error {
	return s.setTrackStatus(trackID, "skipped")
}

// ClearQueue skips every not-yet-played track for an event (organizer-only).
func (s *Service) ClearQueue(eventID string) error {
	_, err := s.sb.Update("music_queue",
		"event_id=eq."+store.Q(eventID)+"&status=in.(pending,queued,playing)",
		map[string]any{"status": "skipped"})
	return err
}

// MarkPlaying moves a track to 'playing' and retires any other playing track to
// 'played' (only one plays at a time). Called by the organizer's TV device.
func (s *Service) MarkPlaying(eventID, trackID string) error {
	_, _ = s.sb.Update("music_queue",
		"event_id=eq."+store.Q(eventID)+"&status=eq.playing",
		map[string]any{"status": "played"})
	return s.setTrackStatus(trackID, "playing")
}

// MarkPlayed retires a finished track.
func (s *Service) MarkPlayed(trackID string) error {
	return s.setTrackStatus(trackID, "played")
}

// SetMusicSettings toggles the jukebox on/off + approval requirement for an
// event (organizer-only).
func (s *Service) SetMusicSettings(eventID string, enabled, requireApproval bool) error {
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID), map[string]any{
		"music_enabled":          enabled,
		"music_require_approval": requireApproval,
	})
	return err
}
