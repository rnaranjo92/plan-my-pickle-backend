package api

import "html/template"

// The coaching pages get their own shell rather than the shared seoHead used by
// event/hub pages.
//
// Those pages are thin on purpose: someone arrives from Google already knowing
// the event, reads four facts, and taps through to register. /coaches is the
// opposite — a cold visitor searching "pickleball lessons near me" has no idea
// what coaching here even is, and the directory can legitimately be empty while
// coaches are being onboarded. A page that has to sell needs room to.
//
// Everything claimed below maps to something that actually ships: video
// feedback threads, drill assignments, the six-skill rubric, availability
// booking, lesson packs, and group classes are all real service calls.

const coachHead = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<meta name="description" content="{{.Description}}">
<link rel="canonical" href="{{.Canonical}}">
<meta property="og:title" content="{{.Title}}">
<meta property="og:description" content="{{.Description}}">
<meta property="og:url" content="{{.Canonical}}">
<meta property="og:type" content="website">
{{if .OGImage}}<meta property="og:image" content="{{.OGImage}}">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:image" content="{{.OGImage}}">{{end}}
<script type="application/ld+json">{{.JSONLD}}</script>
<style>
:root{
  --navy:#16245c; --deep:#0e1733; --green:#4f8b3b; --green-dk:#3d6d2d;
  --ink:#16203a; --muted:#5b6b80; --line:#e2ead6; --bg:#f6faf1;
  --gold:#f5c518; --card:#fff;
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);line-height:1.55;
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
  -webkit-font-smoothing:antialiased}
img{max-width:100%}
a{color:var(--green-dk)}
.wrap{max-width:1040px;margin:0 auto;padding:0 20px}

/* header */
.top{background:var(--bg);border-bottom:1px solid var(--line)}
.top .wrap{display:flex;align-items:center;justify-content:space-between;
  gap:16px;padding-top:14px;padding-bottom:14px}
.brand{color:var(--green);font-weight:800;text-decoration:none;font-size:18px}
.top nav{display:flex;gap:18px;align-items:center;flex-wrap:wrap}
/* :not(.btn) so the nav's link colour can't override a button's own colour —
   ".top nav a" outranks ".btn--green" on specificity, which rendered the CTA's
   label muted grey on green instead of white. */
.top nav a:not(.btn){color:var(--muted);text-decoration:none;font-size:15px;font-weight:600}
.top nav a:not(.btn):hover{color:var(--ink)}
.top nav a.back{color:var(--green-dk);font-weight:700}

/* hero */
.hero{background:linear-gradient(160deg,var(--deep) 0%,var(--navy) 100%);color:#fff;
  padding:64px 0 60px}
.eyebrow{text-transform:uppercase;letter-spacing:.14em;font-size:12px;font-weight:800;
  color:#9fd18a;margin:0 0 14px}
.hero h1{font-size:44px;line-height:1.08;letter-spacing:-.02em;margin:0 0 16px;
  max-width:16ch;text-wrap:balance}
.hero p{font-size:19px;color:#c9d3ea;margin:0 0 28px;max-width:56ch}
.btns{display:flex;gap:12px;flex-wrap:wrap}
.btn{display:inline-block;text-decoration:none;font-weight:800;font-size:16px;
  padding:14px 24px;border-radius:999px}
.btn--gold{background:var(--gold);color:#1b1b1f}
.btn--ghost{background:transparent;color:#fff;box-shadow:inset 0 0 0 2px rgba(255,255,255,.35)}
.btn--green{background:var(--green);color:#fff}

/* sections */
section{padding:56px 0}
.sec-head{max-width:60ch;margin:0 0 30px}
h2{font-size:29px;line-height:1.15;letter-spacing:-.015em;color:var(--navy);
  margin:0 0 10px;text-wrap:balance}
.lede{color:var(--muted);font-size:17px;margin:0}

.grid{display:grid;gap:16px;grid-template-columns:repeat(auto-fit,minmax(240px,1fr))}
.tile{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:22px}
.tile h3{margin:0 0 6px;font-size:17px;color:var(--navy)}
.tile p{margin:0;color:var(--muted);font-size:15px}
.tile .ic{font-size:22px;line-height:1;margin-bottom:12px;display:block}

/* how it works — numbered because the order is real */
.steps{display:grid;gap:16px;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));
  counter-reset:step}
.step{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:22px;
  position:relative}
.step::before{counter-increment:step;content:counter(step);display:flex;
  align-items:center;justify-content:center;width:30px;height:30px;border-radius:50%;
  background:var(--green);color:#fff;font-weight:800;font-size:14px;margin-bottom:12px}
.step h3{margin:0 0 6px;font-size:17px;color:var(--navy)}
.step p{margin:0;color:var(--muted);font-size:15px}

/* coach cards */
.coaches{display:grid;gap:16px;grid-template-columns:repeat(auto-fit,minmax(280px,1fr))}
.coach{background:var(--card);border:1px solid var(--line);border-radius:16px;
  padding:20px;text-decoration:none;color:var(--ink);display:flex;gap:16px;
  align-items:flex-start}
.coach:hover{border-color:#c9dcb6}
.coach img{width:76px;height:76px;border-radius:50%;object-fit:cover;flex:none;
  background:#eef4e6}
.coach h3{margin:0 0 4px;font-size:18px;color:var(--navy)}
.coach .m{margin:1px 0;color:var(--muted);font-size:14px}
.badge{display:inline-block;background:#e9f2df;color:var(--green-dk);font-weight:800;
  font-size:11px;padding:3px 9px;border-radius:999px;vertical-align:2px}
.empty{background:var(--card);border:1px dashed #c9dcb6;border-radius:16px;
  padding:30px;text-align:center}
.empty p{margin:0 0 18px;color:var(--muted);font-size:16px}

/* recruit band */
.band{background:linear-gradient(160deg,var(--deep) 0%,var(--navy) 100%);color:#fff}
.band h2{color:#fff}
.band .lede{color:#c9d3ea}
.band .tile{background:rgba(255,255,255,.06);border-color:rgba(255,255,255,.14)}
.band .tile h3{color:#fff}
.band .tile p{color:#c9d3ea}

/* faq */
details{background:var(--card);border:1px solid var(--line);border-radius:14px;
  padding:16px 18px;margin-bottom:10px}
summary{cursor:pointer;font-weight:700;color:var(--navy);font-size:16px;list-style:none}
summary::-webkit-details-marker{display:none}
summary::after{content:"+";float:right;color:var(--green);font-weight:800}
details[open] summary::after{content:"–"}
details p{margin:10px 0 0;color:var(--muted);font-size:15px}

/* footer */
.foot{border-top:1px solid var(--line);padding:28px 0 46px;color:var(--muted);font-size:14px}
.foot a{color:var(--green-dk)}
.foot nav{display:flex;gap:16px;flex-wrap:wrap;margin-bottom:12px}

@media (max-width:640px){
  .hero{padding:44px 0 40px}
  .hero h1{font-size:33px}
  .hero p{font-size:17px}
  h2{font-size:24px}
  section{padding:40px 0}
}
</style></head><body>
<header class="top"><div class="wrap">
  <a class="brand" href="` + seoCanonicalBase + `">🥒 PlanMyPickle</a>
  <nav>
    <a class="back" href="` + seoCanonicalBase + `">← Main site</a>
    <a href="/coaches">Find a coach</a>
    <a class="btn btn--green" style="padding:9px 18px;font-size:14px" href="/coaches/apply">Apply to coach</a>
  </nav>
</div></header>`

const coachFoot = `<div class="foot"><div class="wrap">
  <p style="margin:0 0 14px"><a href="` + seoCanonicalBase + `"
    style="font-weight:700">← Back to planmypickle.com</a></p>
  <nav>
    <a href="/coaches">Find a coach</a>
    <a href="/coaches/apply">Coach with us</a>
    <a href="` + seoCanonicalBase + `/guides">Guides</a>
    <a href="` + seoCanonicalBase + `/privacy.html">Privacy</a>
    <a href="` + seoCanonicalBase + `/terms.html">Terms</a>
  </nav>
  <p style="margin:0">Powered by <a href="` + seoCanonicalBase + `">PlanMyPickle</a> —
  run pickleball tournaments, leagues and lessons, minus the chaos.</p>
</div></div></body></html>`

var seoCoachHubTmpl = template.Must(template.New("coachhub").Parse(coachHead + `
<div class="hero"><div class="wrap">
  <p class="eyebrow">Pickleball coaching</p>
  <h1>Get coached on the matches you actually play.</h1>
  <p>Book a lesson near you, or send the clip from last night's game and get
  feedback tied to the exact rally — from a coach anywhere. Plus drills to work
  on before you play again.</p>
  <div class="btns">
    {{if .Cards}}<a class="btn btn--gold" href="#directory">Browse coaches</a>{{else}}<a class="btn btn--gold" href="` + seoAppBase + `">Open the app</a>{{end}}
    <a class="btn btn--ghost" href="/coaches/apply">I'm a coach →</a>
  </div>
</div></div>

<section><div class="wrap">
  <div class="sec-head">
    <h2>A lesson that doesn't end when you leave the court</h2>
    <p class="lede">Most coaching stops at the hour you paid for. Here the work
    carries on in the app between sessions.</p>
  </div>
  <div class="grid">
    <div class="tile"><span class="ic">🎥</span>
      <h3>Feedback on your own footage</h3>
      <p>Upload a clip from a real match. Your coach replies on the moment that
      matters, not on a drill you did once in a lesson.</p></div>
    <div class="tile"><span class="ic">🎯</span>
      <h3>Drills assigned to you</h3>
      <p>Your coach assigns drills to practise between sessions, and you tick
      them off as you go.</p></div>
    <div class="tile"><span class="ic">📈</span>
      <h3>Six skills, tracked over time</h3>
      <p>Serve, return, dinks, drops, volleys and strategy — rated by your coach
      so progress is something you can see, not just feel.</p></div>
    <div class="tile"><span class="ic">📅</span>
      <h3>Lessons, clinics and packs</h3>
      <p>Book against your coach's real availability, join a group clinic, or buy
      a block of lessons up front and draw it down.</p></div>
    <div class="tile"><span class="ic">💬</span>
      <h3>A line to your coach</h3>
      <p>Message between sessions and keep shared notes, so the thing you worked
      on last time isn't forgotten by the next one.</p></div>
    <div class="tile"><span class="ic">📓</span>
      <h3>Your practice, logged</h3>
      <p>Record what you actually worked on. Your coach sees it, so the next
      lesson starts where you got to — not where you left off.</p></div>
  </div>
</div></section>

<section style="padding-top:0"><div class="wrap">
  <div class="sec-head">
    <h2>What a month actually looks like</h2>
    <p class="lede">Coaching here isn't a calendar invite and a receipt. This is
    the normal rhythm.</p>
  </div>
  <div class="grid">
    <div class="tile"><span class="ic">1️⃣</span>
      <h3>Week one — the lesson</h3>
      <p>You book an hour. Your coach rates where you are across the six skills,
      and you leave with two drills to practise.</p></div>
    <div class="tile"><span class="ic">2️⃣</span>
      <h3>Week two — the clip</h3>
      <p>You play a real game and send the rally you keep losing. Your coach
      replies on that moment, not on a generic tip.</p></div>
    <div class="tile"><span class="ic">3️⃣</span>
      <h3>Week three — the clinic</h3>
      <p>You join a group session to drill it live with other players at your
      level, usually for less than a private lesson.</p></div>
    <div class="tile"><span class="ic">4️⃣</span>
      <h3>Week four — the proof</h3>
      <p>Your coach re-rates the six skills. You can see which ones moved, which
      is the part most players never get.</p></div>
  </div>
</div></section>

<section style="padding-top:0"><div class="wrap">
  <div class="sec-head"><h2>How it works</h2></div>
  <div class="steps">
    <div class="step"><h3>Find your coach</h3>
      <p>Browse coaches by what they teach and what they charge. Nearby for
      court time, anywhere for video coaching.</p></div>
    <div class="step"><h3>Book a lesson or clinic</h3>
      <p>Pick a time from their calendar, or enrol in a group class. Pay in the
      app.</p></div>
    <div class="step"><h3>Keep working between sessions</h3>
      <p>Send match clips, get feedback and drills, and watch your skill ratings
      move.</p></div>
  </div>
</div></section>

<section id="directory" style="padding-top:0"><div class="wrap">
  <div class="sec-head">
    <h2>{{if .Cards}}Coaches on PlanMyPickle{{else}}Coaches are joining now{{end}}</h2>
    <p class="lede">{{.Intro}}</p>
  </div>
  {{if .Cards}}
  <div class="coaches">
  {{range .Cards}}<a class="coach" href="{{.URL}}">
    {{if .Photo}}<img src="{{.Photo}}" alt="{{.Name}}" loading="lazy" width="76" height="76">{{end}}
    <div>
      <h3>{{.Name}}{{if .Verified}} <span class="badge">Verified</span>{{end}}</h3>
      {{if .City}}<p class="m">📍 {{.City}}</p>{{end}}
      {{if .Skills}}<p class="m">🎾 {{.Skills}}</p>{{end}}
      {{if .Rate}}<p class="m">💵 {{.Rate}}</p>{{end}}
    </div>
  </a>{{end}}
  </div>
  {{else}}
  <div class="empty">
    <p>We review every coach by hand before listing them, so this page fills up
    slowly on purpose. If you teach pickleball — anywhere — this is the front
    door.</p>
    <a class="btn btn--green" href="/coaches/apply">Apply to coach →</a>
  </div>
  {{end}}
</div></section>

<div class="band"><section><div class="wrap">
  <div class="sec-head">
    <p class="eyebrow">For coaches</p>
    <h2>Teach more. Admin less.</h2>
    <p class="lede">Bring the students you already have, or get found by new
    ones — nearby for lessons, anywhere for video. Either way the busywork moves
    into the app.</p>
  </div>
  <div class="grid">
    <div class="tile"><h3>Your roster in one place</h3>
      <p>Invite students, keep notes, and see who's gone quiet — without a
      spreadsheet or a group chat.</p></div>
    <div class="tile"><h3>Get paid without chasing</h3>
      <p>Lessons, clinics and lesson packs are paid in the app, so you're not
      collecting cash at the net.</p></div>
    <div class="tile"><h3>Coach between lessons</h3>
      <p>Review student clips when it suits you, assign drills in a tap, and let
      the ratings show parents and players the progress.</p></div>
  </div>
  <div class="btns" style="margin-top:26px">
    <a class="btn btn--gold" href="/coaches/apply">Apply to coach →</a>
  </div>
</div></section></div>

<section><div class="wrap">
  <div class="sec-head"><h2>Common questions</h2></div>
  <details><summary>How much does a lesson cost?</summary>
    <p>Coaches set their own rates, so it varies. Each coach's hourly rate is on
    their profile before you book anything.</p></details>
  <details><summary>Do I need to send video?</summary>
    <p>No. Video feedback is there if you want it — plenty of players just book
    lessons and clinics. But it's the part most people find hardest to get
    anywhere else.</p></details>
  <details><summary>How do I pay?</summary>
    <p>In the app, at the time you book — a single lesson, a spot in a clinic, or
    a pack of lessons up front. Your coach sets the price and you see it before
    you commit. No cash at the net.</p></details>
  <details><summary>What if I have to cancel?</summary>
    <p>Each coach sets their own cancellation policy, shown on their profile
    before you book.</p></details>
  <details><summary>How are coaches vetted?</summary>
    <p>Every coach applies and is reviewed by hand before they appear here. We
    ask about certifications, coaching history and insurance, and only listed,
    approved coaches show on this page.</p></details>
  <details><summary>I'm a coach — what does it cost me?</summary>
    <p>Applying is free. Start by <a href="/coaches/apply">sending an
    application</a> and we'll walk you through the rest.</p></details>
  <details><summary>Where is this available?</summary>
    <p>Anywhere. Video feedback, drills and skill tracking work wherever you
    play — your coach doesn't have to be in your city. In-person lessons and
    clinics depend on a coach near you, and we're onboarding coaches
    everywhere, so apply or open the app to see who's closest.</p></details>
  <details><summary>Can my coach be in a different city?</summary>
    <p>Yes. Plenty of coaching here happens on video: you send clips from your
    matches, your coach responds on the exact rally and assigns drills. If you
    also want court time, filter for coaches near you.</p></details>
</div></section>
` + coachFoot))

var seoCoachTmpl = template.Must(template.New("coach").Parse(coachHead + `
<div class="hero"><div class="wrap">
  <p class="eyebrow">Pickleball coach{{if .CityLine}} · {{.CityLine}}{{end}}</p>
  <h1>{{.H1}}</h1>
  {{if .Bio}}<p>{{.Bio}}</p>{{end}}
  <div class="btns">
    <a class="btn btn--gold" href="{{.BookURL}}">Book a lesson →</a>
    <a class="btn btn--ghost" href="/coaches">All coaches</a>
  </div>
</div></div>

<section><div class="wrap">
  <div class="grid">
    {{if .Verified}}<div class="tile"><span class="ic">✅</span>
      <h3>Verified coach</h3>
      <p>Reviewed and approved by PlanMyPickle before being listed.</p></div>{{end}}
    {{if .RateLine}}<div class="tile"><span class="ic">💵</span>
      <h3>{{.RateLine}}</h3><p>Their standard hourly rate.</p></div>{{end}}
    {{if .ExpLine}}<div class="tile"><span class="ic">⏳</span>
      <h3>{{.ExpLine}}</h3><p>Time spent coaching players.</p></div>{{end}}
    {{if .SkillsLine}}<div class="tile"><span class="ic">🎾</span>
      <h3>Teaches</h3><p>{{.SkillsLine}}</p></div>{{end}}
    {{if .CertLine}}<div class="tile"><span class="ic">🎓</span>
      <h3>Certifications</h3><p>{{.CertLine}}</p></div>{{end}}
    {{if .CityLine}}<div class="tile"><span class="ic">📍</span>
      <h3>Based in {{.CityLine}}</h3><p>Lessons and clinics in the area.</p></div>{{end}}
  </div>
</div></section>

<section style="padding-top:0"><div class="wrap">
  <div class="sec-head">
    <h2>What coaching with {{.H1}} includes</h2>
    <p class="lede">Booking, video feedback and drills all run in the
    PlanMyPickle app.</p>
  </div>
  <div class="steps">
    <div class="step"><h3>Book a time</h3>
      <p>Pick a slot from their availability, or join one of their group
      clinics.</p></div>
    <div class="step"><h3>Send your match clips</h3>
      <p>Upload footage from a real game and get feedback on the rally that
      matters.</p></div>
    <div class="step"><h3>Practise with a plan</h3>
      <p>Drills assigned between sessions, and skill ratings that show how far
      you've come.</p></div>
  </div>
  <div class="btns" style="margin-top:26px">
    <a class="btn btn--green" href="{{.BookURL}}">Book a lesson →</a>
  </div>
</div></section>
` + coachFoot))
