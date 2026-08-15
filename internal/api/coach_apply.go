package api

import (
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// The public "apply to coach" page and its endpoints.
//
// Coach access comes from the `instructors` allowlist and always has — nobody
// can list themselves. What was missing was a way IN: a coach had to know to
// email someone, and nothing survived the conversation. This is the front door.
//
// Server-rendered rather than a Flutter route on purpose: it's a page you send
// to someone who has never used PlanMyPickle, often from a phone, and asking
// them to load an app shell to fill in six fields is how an application doesn't
// get filled in.

func (s *Server) registerCoachApply(mux *http.ServeMux) {
	mux.HandleFunc("GET /coaches/apply", s.coachApplyPage)
	mux.HandleFunc("POST /coaches/apply", s.coachApplySubmit)
	// JSON submit, for the app or anything else that wants it.
	mux.HandleFunc("POST /public/coach-applications", s.coachApplyJSON)
	// Owner-only review.
	mux.HandleFunc("GET /coach-applications",
		s.ownerEmailOnly(s.listCoachApplications))
	mux.HandleFunc("POST /coach-applications/{id}/decide",
		s.ownerEmailOnly(s.decideCoachApplication))
}

func (s *Server) coachApplyPage(w http.ResponseWriter, r *http.Request) {
	s.renderCoachApply(w, "", "")
}

// coachApplySubmit handles the HTML form post and re-renders the same page with
// the outcome — no redirect, so a failed submission keeps what they typed.
func (s *Server) coachApplySubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderCoachApply(w, "", "Something went wrong — please try again.")
		return
	}
	app := model.CoachApplication{
		Email:          r.FormValue("email"),
		Name:           r.FormValue("name"),
		Phone:          r.FormValue("phone"),
		City:           r.FormValue("city"),
		Certifications: r.FormValue("certifications"),
		Experience:     r.FormValue("experience"),
		HasInsurance:   r.FormValue("insurance") == "yes",
		Note:           r.FormValue("note"),
	}
	if err := s.svc.SubmitCoachApplication(app); err != nil {
		s.renderCoachApply(w, "", err.Error())
		return
	}
	s.renderCoachApply(w,
		"Thanks — your application is in. We'll email you at "+
			html.EscapeString(strings.TrimSpace(app.Email))+" once it's reviewed.",
		"")
}

func (s *Server) coachApplyJSON(w http.ResponseWriter, r *http.Request) {
	var app model.CoachApplication
	if !decode(w, r, &app) {
		return
	}
	if err := s.svc.SubmitCoachApplication(app); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listCoachApplications(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.ListCoachApplications(r.URL.Query().Get("status"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) decideCoachApplication(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Decision string `json:"decision"` // approved | rejected
		Note     string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.DecideCoachApplication(
		r.PathValue("id"), req.Decision, req.Note, userEmail(r)); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renderCoachApply writes the page. [ok] and [errMsg] are mutually exclusive.
func (s *Server) renderCoachApply(w http.ResponseWriter, ok, errMsg string) {
	banner := ""
	if ok != "" {
		banner = `<div class="banner good">` + ok + `</div>`
	} else if errMsg != "" {
		banner = `<div class="banner bad">` + html.EscapeString(errMsg) + `</div>`
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	// No-cache: the page carries a submission outcome.
	w.Header().Set("cache-control", "no-store")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Coach on PlanMyPickle</title>
<meta name="description" content="Apply to teach pickleball on PlanMyPickle — run classes, give video feedback, and get found by players near you.">
<style>
  :root { --navy:#16245C; --green:#2E7D32; --ink:#1B1B1F; --muted:#6B7280;
          --line:#E5E7EB; --bg:#F7F8FA; }
  * { box-sizing:border-box; }
  body { margin:0; background:var(--bg); color:var(--ink);
         font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif; }
  .wrap { max-width:640px; margin:0 auto; padding:28px 20px 64px; }
  h1 { font-size:30px; line-height:1.15; margin:0 0 10px; color:var(--navy); }
  .sub { color:var(--muted); margin:0 0 26px; }
  .card { background:#fff; border:1px solid var(--line); border-radius:16px;
          padding:22px; }
  label { display:block; font-weight:700; font-size:14px; margin:16px 0 6px; }
  .hint { font-weight:400; color:var(--muted); font-size:13px; }
  input[type=text], input[type=email], input[type=tel], textarea {
    width:100%%; padding:11px 12px; border:1px solid var(--line);
    border-radius:10px; font:inherit; background:#fff; }
  textarea { min-height:90px; resize:vertical; }
  .row { display:flex; gap:12px; }
  .row > div { flex:1; }
  .check { display:flex; gap:10px; align-items:flex-start; margin-top:16px; }
  button { margin-top:22px; width:100%%; padding:14px; border:0; border-radius:12px;
           background:var(--green); color:#fff; font-size:16px; font-weight:800; }
  .banner { padding:12px 14px; border-radius:12px; margin-bottom:18px;
            font-weight:600; }
  .good { background:#E8F5E9; color:#1B5E20; }
  .bad { background:#FDECEA; color:#B3261E; }
  .foot { color:var(--muted); font-size:13px; margin-top:18px; }
</style>
</head><body><div class="wrap">
  <h1>Coach on PlanMyPickle</h1>
  <p class="sub">Run classes, give video feedback on your students' matches, and
  get found by players near you. Tell us about yourself and we'll be in touch.</p>
  %s
  <form class="card" method="post" action="/coaches/apply">
    <div class="row">
      <div>
        <label>Your name<input type="text" name="name" required autocomplete="name"></label>
      </div>
      <div>
        <label>City <span class="hint">where you teach</span>
          <input type="text" name="city" autocomplete="address-level2"></label>
      </div>
    </div>
    <div class="row">
      <div>
        <label>Email <span class="hint">we'll use this for your account</span>
          <input type="email" name="email" required autocomplete="email"></label>
      </div>
      <div>
        <label>Phone <span class="hint">optional</span>
          <input type="tel" name="phone" autocomplete="tel"></label>
      </div>
    </div>
    <label>Certifications
      <span class="hint">PPR, IPTPA, PTR — a number we can verify, if you have one</span>
      <input type="text" name="certifications"></label>
    <label>Coaching experience
      <span class="hint">where you've taught, how long, who you work with</span>
      <textarea name="experience"></textarea></label>
    <div class="check">
      <input type="checkbox" id="ins" name="insurance" value="yes">
      <label for="ins" style="margin:0">I carry liability insurance
        <span class="hint">not required to apply, but it helps</span></label>
    </div>
    <label>Anything else
      <span class="hint">optional</span>
      <textarea name="note"></textarea></label>
    <button type="submit">Send application</button>
    <p class="foot">We review every application by hand. You'll hear from us by
    email — usually within a few days.</p>
  </form>
</div></body></html>`, banner)
}
