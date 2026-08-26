package virtualmetadata

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remuxdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SyncRemuxDBOnStartup scans virtual media files in SiloDB that do not yet
// have recorded stream metadata, fetches crowdsourced probe evidence from
// RemuxDB in the background, and populates virtual_stream_metadata locally.
func SyncRemuxDBOnStartup(ctx context.Context, db *pgxpool.Pool, store Store, client *remuxdb.Client) {
	if db == nil || store == nil || client == nil {
		return
	}
	go func() {
		// Bounded background context detached from startup
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
		defer cancel()

		rows, err := db.Query(syncCtx, `
			SELECT DISTINCT m.content_id, m.file_path, COALESCE(m.season_number, 0), COALESCE(m.episode_number, 0)
			FROM media_files m
			LEFT JOIN virtual_stream_metadata v ON m.content_id = v.content_id
			WHERE (m.file_path LIKE 'virtual://%' OR m.container = 'virtual')
			  AND m.content_id <> ''
			  AND v.content_id IS NULL
			LIMIT 500`)
		if err != nil {
			slog.DebugContext(syncCtx, "remuxdb startup sync query failed", "component", "virtualmetadata", "error", err)
			return
		}
		defer rows.Close()

		type itemToSync struct {
			contentID string
			filePath  string
			season    int
			episode   int
		}
		var items []itemToSync
		for rows.Next() {
			var it itemToSync
			if scanErr := rows.Scan(&it.contentID, &it.filePath, &it.season, &it.episode); scanErr == nil {
				items = append(items, it)
			}
		}

		if len(items) == 0 {
			return
		}

		slog.InfoContext(syncCtx, "remuxdb startup sync starting", "component", "virtualmetadata", "count", len(items))
		synced := 0

		for _, it := range items {
			select {
			case <-syncCtx.Done():
				return
			default:
			}

			imdbID := remuxdb.ExtractIMDbID(it.contentID)
			if imdbID == "" {
				imdbID = remuxdb.ExtractIMDbID(it.filePath)
			}
			if imdbID == "" {
				continue
			}

			var seasonPtr, epPtr *int
			if it.season > 0 {
				s := it.season
				seasonPtr = &s
			}
			if it.episode > 0 {
				e := it.episode
				epPtr = &e
			}

			reqCtx, reqCancel := context.WithTimeout(syncCtx, 4*time.Second)
			infos, fetchErr := client.FetchProbe(reqCtx, imdbID, seasonPtr, epPtr)
			reqCancel()

			if fetchErr == nil && len(infos) > 0 {
				dummyFile := &models.MediaFile{
					ContentID: it.contentID,
					FilePath:  it.filePath,
				}
				if remuxdb.PopulateMediaFileFromRemuxDB(infos[0], dummyFile) {
					rangeName := "sdr"
					if dummyFile.HDR {
						rangeName = "hdr"
					}
					if len(dummyFile.VideoTracks) > 0 && dummyFile.VideoTracks[0].VideoRange != "" {
						rangeName = strings.ToLower(dummyFile.VideoTracks[0].VideoRange)
					}
					cid := it.contentID
					if it.season > 0 || it.episode > 0 {
						cid = fmt.Sprintf("%s-s%de%d", it.contentID, it.season, it.episode)
					}
					recErr := store.Record(syncCtx, Evidence{
						ContentID:  cid,
						Container:  dummyFile.Container,
						CodecVideo: dummyFile.CodecVideo,
						CodecAudio: dummyFile.CodecAudio,
						VideoRange: rangeName,
						Resolution: dummyFile.Resolution,
					})
					if recErr == nil {
						synced++
					}
				}
			}

			// Polite pacing: 50ms pause between RemuxDB requests
			time.Sleep(50 * time.Millisecond)
		}

		if synced > 0 {
			slog.InfoContext(syncCtx, "remuxdb startup sync finished", "component", "virtualmetadata", "synced", synced)
		}
	}()
}
