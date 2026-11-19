package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
)

// consumer is a caller of the API; its token is in the environment under the Cove key Key.
type consumer struct {
	Name string
	Key  string
}

// consumers are the API's callers; adding one takes a line here, a Cove key and a compose line.
var consumers = []consumer{
	{Name: "vista", Key: "SHARED_LIGHTHOUSE_VISTA_TOKEN"},
}

// minTokenLength is the shortest token the API accepts from Cove.
const minTokenLength = 32

// credential is a consumer and its token's digest; the token itself isn't kept.
type credential struct {
	consumer consumer
	digest   [sha256.Size]byte
}

// loadCredentials reads each token, refusing a missing or short one so a deploy without it fails.
func loadCredentials(getenv func(string) string, list []consumer) ([]credential, error) {
	creds := make([]credential, 0, len(list))
	for _, c := range list {
		token := strings.TrimSpace(getenv(c.Key))
		switch {
		case token == "":
			return nil, fmt.Errorf("api: %s's token, %s, isn't set: create it in Cove (generate %s 64) and pass it through in docker-compose.yml", c.Name, c.Key, c.Key)
		case len(token) < minTokenLength:
			return nil, fmt.Errorf("api: %s's token, %s, is %d characters; it needs at least %d: generate a new one in Cove (generate %s 64)", c.Name, c.Key, len(token), minTokenLength, c.Key)
		}
		creds = append(creds, credential{consumer: c, digest: sha256.Sum256([]byte(token))})
	}
	return creds, nil
}

// identify returns the token's consumer, comparing every digest in constant time.
func identify(creds []credential, token string) (consumer, bool) {
	digest := sha256.Sum256([]byte(token))
	var found consumer
	ok := false
	for _, c := range creds {
		if subtle.ConstantTimeCompare(digest[:], c.digest[:]) == 1 {
			found, ok = c.consumer, true
		}
	}
	return found, ok
}
