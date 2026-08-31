package store

import (
	"database/sql"
	"errors"
	"time"
)

type PasskeyCredentialRecord struct {
	CredentialID string
	Data         string
}

func (s *Store) PasskeyCount() int {
	var count int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM passkey_credentials`).Scan(&count)
	return count
}

func (s *Store) PasskeyCredentials() ([]PasskeyCredentialRecord, error) {
	rows, err := s.DB.Query(`SELECT credential_id,credential_data FROM passkey_credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	credentials := make([]PasskeyCredentialRecord, 0)
	for rows.Next() {
		var record PasskeyCredentialRecord
		var sealed string
		if err := rows.Scan(&record.CredentialID, &sealed); err != nil {
			return nil, err
		}
		data, err := s.OpenSecret(sealed)
		if err != nil {
			return nil, err
		}
		record.Data = data
		credentials = append(credentials, record)
	}
	return credentials, rows.Err()
}

func (s *Store) SavePasskeyCredential(credentialID, data string) error {
	sealed, err := s.Seal(data)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO passkey_credentials(credential_id,credential_data,created_at,last_used_at) VALUES(?,?,?,0)`, credentialID, sealed, time.Now().Unix())
	return err
}

func (s *Store) UpdatePasskeyCredential(credentialID, data string) error {
	sealed, err := s.Seal(data)
	if err != nil {
		return err
	}
	result, err := s.DB.Exec(`UPDATE passkey_credentials SET credential_data=?,last_used_at=? WHERE credential_id=?`, sealed, time.Now().Unix(), credentialID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return errors.New("passkey credential not found")
	}
	return nil
}

func (s *Store) SavePasskeyChallenge(id, kind, sessionID, data string, ttl time.Duration) error {
	sealed, err := s.Seal(data)
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = s.DB.Exec(`INSERT INTO passkey_challenges(id,kind,session_id,session_data,created_at,expires_at) VALUES(?,?,?,?,?,?)`, id, kind, sessionID, sealed, now.Unix(), now.Add(ttl).Unix())
	return err
}

// ConsumePasskeyChallenge makes every WebAuthn ceremony one-time use, even if
// two finish requests arrive concurrently.
func (s *Store) ConsumePasskeyChallenge(id, kind, sessionID string) (string, bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var data string
	var expiresAt int64
	err = tx.QueryRow(`SELECT session_data,expires_at FROM passkey_challenges WHERE id=? AND kind=? AND session_id=?`, id, kind, sessionID).Scan(&data, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if expiresAt < time.Now().Unix() {
		_, _ = tx.Exec(`DELETE FROM passkey_challenges WHERE id=?`, id)
		_ = tx.Commit()
		return "", false, nil
	}
	if _, err = tx.Exec(`DELETE FROM passkey_challenges WHERE id=?`, id); err != nil {
		return "", false, err
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	opened, err := s.OpenSecret(data)
	if err != nil {
		return "", false, err
	}
	return opened, true, nil
}
