package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type DatabaseMatcher struct{ DB *sql.DB }

const matchingQuery = `
SELECT item.content_id, item.type, COALESCE(item.tmdb_id, ''),
       COALESCE(item.imdb_id, ''), COALESCE(item.tvdb_id, ''),
       COALESCE(playable.episode_id, '')
FROM public.media_items AS item
JOIN LATERAL (
    SELECT file.episode_id
    FROM public.media_files AS file
		%EPISODE_JOIN%
    WHERE file.media_folder_id = $3::integer AND file.missing_since IS NULL
			AND %PLAYABLE_MATCH%
    ORDER BY file.id
    LIMIT 1
) AS playable ON true
WHERE item.type = $1 AND %s = $2
ORDER BY item.content_id
LIMIT 201`

func (matcher *DatabaseMatcher) Lookup(ctx context.Context, libraryID, mediaType string, entries []Entry) ([]CatalogEntry, error) {
	items := []CatalogEntry{}
	if len(entries) == 0 {
		return items, nil
	}
	if len(entries) > 200 || !positiveID.MatchString(libraryID) {
		return nil, errors.New("invalid bounded database lookup")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	transaction, err := matcher.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, errors.New("read-only database transaction unavailable")
	}
	defer transaction.Rollback()
	seen := map[string]bool{}
	for _, entry := range entries {
		query, err := providerQuery(entry, mediaType)
		if err != nil {
			return nil, err
		}
		rows, err := transaction.QueryContext(ctx, query, mediaType, entry.MediaID, libraryID)
		if err != nil {
			return nil, errors.New("targeted database lookup failed")
		}
		count := 0
		for rows.Next() {
			var item CatalogEntry
			if err := rows.Scan(&item.ID, &item.Type, &item.TMDB, &item.IMDB, &item.TVDB, &item.PlayContentID); err != nil {
				rows.Close()
				return nil, errors.New("invalid database match row")
			}
			count++
			if !seen[item.ID] {
				seen[item.ID] = true
				items = append(items, item)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || count > 200 {
			return nil, errors.New("incomplete or oversized database matches")
		}
	}
	if err := transaction.Commit(); err != nil {
		return nil, errors.New("database lookup transaction failed")
	}
	return items, nil
}

func providerQuery(entry Entry, mediaType string) (string, error) {
	kind := entry.MediaType
	if kind == "tv" {
		kind = "series"
	}
	if !validIdentity(entry) || kind != mediaType {
		return "", errors.New("invalid typed provider lookup")
	}
	column := map[string]string{"tmdb": "item.tmdb_id", "imdb": "item.imdb_id", "tvdb": "item.tvdb_id"}[entry.Source]
	episodeJoin, playableMatch := "", "file.content_id = item.content_id"
	if mediaType == "series" {
		episodeJoin = "JOIN public.episodes AS episode ON episode.content_id = file.episode_id"
		playableMatch = "episode.series_id = item.content_id"
	}
	return strings.NewReplacer("%s", column, "%EPISODE_JOIN%", episodeJoin, "%PLAYABLE_MATCH%", playableMatch).Replace(matchingQuery), nil
}
