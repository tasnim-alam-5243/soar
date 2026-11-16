package api

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/security"
)

var secretNameRe = regexp.MustCompile(`^(env:[A-Za-z_][A-Za-z0-9_]{0,99}|[A-Za-z0-9_.-]{1,100})$`)

type connectionIn struct {
	ConnectorID string         `json:"connector_id"`
	Name        string         `json:"name"`
	AuthType    string         `json:"auth_type"`
	SecretRefs  map[string]any `json:"secret_refs"`
	Config      map[string]any `json:"config"`
}

// createConnection stores how to reach an external system. Credentials are never
// part of the request: secret_refs only names secrets (managed in Step 9).
func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) error {
	return createResource(s, w, r, s.st.Connections, "/connections", func() (domain.Connection, string, error) {
		var in connectionIn
		if err := decode(r, &in); err != nil {
			return domain.Connection{}, "", err
		}
		var v verr
		v.required("connector_id", in.ConnectorID)
		v.maxLen("connector_id", in.ConnectorID, 100)
		v.required("name", in.Name)
		v.maxLen("name", in.Name, 200)
		v.oneOf("auth_type", in.AuthType, "none", "api_key", "basic", "bearer", "oauth2_client_credentials")
		for k, val := range in.SecretRefs {
			ref, ok := val.(string)
			switch {
			case !ok || !secretNameRe.MatchString(ref):
				v.add("secret_refs."+k, "must be the name of a secret (letters, digits, . _ -), never the secret itself")
			case strings.HasPrefix(ref, "env:") && !security.EnvRefAllowed(strings.TrimPrefix(ref, "env:"), security.UseConnection):
				// the process environment also holds the platform's own keys
				v.add("secret_refs."+k, "%s", security.EnvRefRule(security.UseConnection))
			}
		}
		if err := v.err(); err != nil {
			return domain.Connection{}, "", err
		}
		doc := domain.Connection{
			Meta: domain.NewMeta("con"), ConnectorID: in.ConnectorID, Name: in.Name,
			AuthType: in.AuthType, SecretRefs: in.SecretRefs, Config: in.Config,
		}
		return doc, doc.ID, nil
	})
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{
		fields:      map[string]string{"created_at": "created_at", "name": "name"},
		defaultName: "created_at", defaultDesc: true,
	}, "connector_id")
	if err != nil {
		return err
	}
	term(&q, v, "connector_id", "connector_id")
	page, err := s.st.Connections.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getConnection(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Connections)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) error {
	if err := s.st.Connections.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
