// ssh_identity.go — HTTP layer for /v1/knowledge/ssh-identities
// Mounted on the knowledge router so the "代码仓库" page
// has a single URL prefix to talk to (matches the singpchia ask
// "all git stuff on that one page").
package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/knowledge"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/knowledge"
)

// sshIdentityDTO is the public view of a stored identity. private_key
// and passphrase are never serialised — even admins only ever see the
// public key + fingerprint after creation.
type sshIdentityDTO struct {
	ID          uint64     `json:"id"`
	Name        string     `json:"name"`
	PublicKey   string     `json:"public_key"`
	Fingerprint string     `json:"fingerprint"`
	Hosts       []string   `json:"hosts"`
	KnownHosts  string     `json:"known_hosts,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func toSSHIdentityDTO(row *model.SSHIdentity) sshIdentityDTO {
	hosts := []string{}
	if row.HostsJSON != "" {
		_ = json.Unmarshal([]byte(row.HostsJSON), &hosts)
	}
	return sshIdentityDTO{
		ID:          row.ID,
		Name:        row.Name,
		PublicKey:   row.PublicKey,
		Fingerprint: row.Fingerprint,
		Hosts:       hosts,
		KnownHosts:  row.KnownHosts,
		LastUsedAt:  row.LastUsedAt,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

type createSSHIdentityReq struct {
	Name       string   `json:"name"`
	PrivateKey string   `json:"private_key"`
	Hosts      []string `json:"hosts"`
	KnownHosts string   `json:"known_hosts,omitempty"`
}

type updateSSHIdentityReq struct {
	Name       string   `json:"name"`
	Hosts      []string `json:"hosts"`
	KnownHosts string   `json:"known_hosts"`
}

type generateSSHIdentityReq struct {
	Name       string   `json:"name"`
	Hosts      []string `json:"hosts"`
	KnownHosts string   `json:"known_hosts,omitempty"`
}

func (h *Handler) listSSHIdentities(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListSSHIdentities(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]sshIdentityDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, toSSHIdentityDTO(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out)})
}

// sshKeyEvidence is what an SSH identity row records, and it is defined by what
// it refuses (决策 336).
//
// **Never the key material.** Not the private key (that is the whole point of
// the row existing) and not the public key either, because the public key is a
// few hundred characters of base64 that would sit in an append-only chain
// forever for no analytical gain. What replaces them:
//
//	fingerprint — the stable public identity of the key. It answers "is this the
//	  same key as last quarter" without being usable for anything, which is
//	  exactly the property a chain needs.
//	hosts — the question that matters when an identity is suspected is "which
//	  machines could this key open", not "what does the key look like".
type sshKeyEvidence struct {
	Fingerprint string   `json:"fingerprint,omitempty"`
	Hosts       []string `json:"hosts,omitempty"`
}

func sshEvidenceOf(row *model.SSHIdentity) sshKeyEvidence {
	ev := sshKeyEvidence{Fingerprint: row.Fingerprint}
	if row.HostsJSON != "" {
		_ = json.Unmarshal([]byte(row.HostsJSON), &ev.Hosts)
	}
	return ev
}

func (h *Handler) createSSHIdentity(w http.ResponseWriter, r *http.Request) {
	var req createSSHIdentityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	row, err := h.svc.CreateSSHIdentity(r.Context(), biz.CreateSSHIdentityInput{
		Name:       req.Name,
		PrivateKey: req.PrivateKey,
		Hosts:      req.Hosts,
		KnownHosts: req.KnownHosts,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 336：请求体里就是那把私钥，响应体里是它的公钥——**两样都不进链**。
	// 留下指纹与主机列表：前者回答「还是同一把钥匙吗」，后者回答「这把钥匙能
	// 打开哪些机器」，而后者才是怀疑某把身份时真正要问的那句。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionSSHKeyRegister,
		ResourceType: auditport.ResourceGitKey,
		ResourceID:   strconv.FormatUint(row.ID, 10),
		ResourceName: row.Name,
		Status:       auditport.StatusSuccess,
		Payload:      sshEvidenceOf(row),
	})
	writeJSON(w, http.StatusCreated, toSSHIdentityDTO(row))
}

func (h *Handler) generateSSHIdentity(w http.ResponseWriter, r *http.Request) {
	var req generateSSHIdentityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	row, err := h.svc.GenerateSSHIdentity(r.Context(), biz.GenerateSSHIdentityInput{
		Name:       req.Name,
		Hosts:      req.Hosts,
		KnownHosts: req.KnownHosts,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 201 with the public_key prominently in the response — the SPA
	// shows it in a copyable block immediately so the operator can
	// paste it into the host's Deploy keys list without an extra
	// round-trip.
	//
	// 决策 336：这一行的份量比它的邻居都重，因为**私钥是平台自己铸的**，而且
	// 从此只出现一次——就在这个响应体里。丢了就只能重来。所以「谁在什么时候铸了
	// 现在挂在这些机器上的那把钥匙」这件事，除了这一行没有第二个来源。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionSSHKeyGenerate,
		ResourceType: auditport.ResourceGitKey,
		ResourceID:   strconv.FormatUint(row.ID, 10),
		ResourceName: row.Name,
		Status:       auditport.StatusSuccess,
		Payload:      sshEvidenceOf(row),
	})
	writeJSON(w, http.StatusCreated, toSSHIdentityDTO(row))
}

func (h *Handler) updateSSHIdentity(w http.ResponseWriter, r *http.Request) {
	id, err := parseUintParam(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var req updateSSHIdentityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	row, err := h.svc.UpdateSSHIdentity(r.Context(), id, biz.UpdateSSHIdentityInput{
		Name:       req.Name,
		Hosts:      req.Hosts,
		KnownHosts: req.KnownHosts,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 336：这一行是「这把钥匙现在能打开哪些机器」的变更记录。改 hosts
	// 等于扩大一把私钥的可达范围——**这是一次授权变更，尽管它的界面长得像编辑表单。**
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionSSHKeyUpdate,
		ResourceType: auditport.ResourceGitKey,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: row.Name,
		Status:       auditport.StatusSuccess,
		Payload:      sshEvidenceOf(row),
	})
	writeJSON(w, http.StatusOK, toSSHIdentityDTO(row))
}

func (h *Handler) deleteSSHIdentity(w http.ResponseWriter, r *http.Request) {
	id, err := parseUintParam(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	// 名字与指纹先读：删除之后链上剩下的只有一个数字 id，而「删掉的是哪把钥匙、
	// 它能打开哪些机器」正是事后第一个要回答的问题。读不到不是不写这行的理由。
	var name, fingerprint string
	var hosts []string
	if all, err := h.svc.ListSSHIdentities(r.Context()); err == nil {
		for _, row := range all {
			if row.ID == id {
				name = row.Name
				ev := sshEvidenceOf(row)
				fingerprint, hosts = ev.Fingerprint, ev.Hosts
				break
			}
		}
	}
	if err := h.svc.DeleteSSHIdentity(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionSSHKeyDelete,
		ResourceType: auditport.ResourceGitKey,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: name,
		Status:       auditport.StatusSuccess,
		Payload:      sshKeyEvidence{Fingerprint: fingerprint, Hosts: hosts},
	})
	w.WriteHeader(http.StatusNoContent)
}

func parseUintParam(r *http.Request, key string) (uint64, error) {
	raw := chi.URLParam(r, key)
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.Join(errs.ErrInvalid, err)
	}
	return n, nil
}

// Ensure context is imported even if no direct usage in this file's
// helpers — gives compiler stable surface across edits.
var _ = context.TODO
