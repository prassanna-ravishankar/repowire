package state

import (
	"context"
	"fmt"
	"strings"
)

// AttachmentRef is one attachment a stored message still carries: the id
// and path its upload returned, as the sender passed them on.
type AttachmentRef struct {
	ID   string
	Path string
}

// attachmentHolders are the tables whose rows carry attachments to a reader:
// an agent's message, the human's inbox item, a delivery waiting to be sent.
// A row holds its attachments until retention strips its content (or the
// queue drops it), so these rows are every reference a file still has.
var attachmentHolders = []string{"peer_messages", "inbox_items", "queued_deliveries"}

// AttachmentReferences lists the attachments the store's messages still
// carry. A file is in use while one of them names it, however long ago it
// was uploaded: its life runs from its latest reference, not its upload.
func (s *Store) AttachmentReferences(ctx context.Context) ([]AttachmentRef, error) {
	parts := make([]string, len(attachmentHolders))
	for i, table := range attachmentHolders {
		// Static table names; a value that is not an object yields NULLs.
		parts[i] = fmt.Sprintf(`SELECT json_extract(a.value, '$.id'), json_extract(a.value, '$.path')
			FROM %s, json_each(%s.attachments_json) a
			WHERE %s.attachments_json != '[]' AND json_valid(%s.attachments_json) AND a.type = 'object'`, table, table, table, table)
	}
	rows, err := s.db.QueryContext(ctx, strings.Join(parts, " UNION "))
	if err != nil {
		return nil, fmt.Errorf("attachment references: %w", err)
	}
	defer rows.Close()
	var refs []AttachmentRef
	for rows.Next() {
		var id, path any
		if err := rows.Scan(&id, &path); err != nil {
			return nil, fmt.Errorf("attachment references: %w", err)
		}
		ref := AttachmentRef{ID: textOf(id), Path: textOf(path)}
		if ref.ID != "" || ref.Path != "" {
			refs = append(refs, ref)
		}
	}
	return refs, rows.Err()
}

// textOf is a JSON scalar SQLite returned as a string, "" for anything else.
func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return ""
}
