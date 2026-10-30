package render

// incidentTemplate is the interactive triage email. Inline styles only (email
// clients strip stylesheets), semantic headings and real link text so screen
// readers and text-only clients both work.
const incidentTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>WatchTower incident {{.Incident.ID}}</title>
</head>
<body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1b1f23;">
<div style="max-width:720px;margin:0 auto;padding:24px;">

  <h1 style="font-size:20px;margin:0 0 4px;">{{.Incident.Title}}</h1>
  <p style="margin:0 0 16px;color:#57606a;font-size:14px;">
    Incident <strong>{{.Incident.ID}}</strong> &middot; severity {{upper (printf "%s" .Incident.Severity)}} &middot;
    target {{.Incident.PrimaryAlert.Target}} &middot;
    confidence {{pct .Confidence}} ({{.Badge}})
  </p>

  {{if .Plan}}
  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Diagnosis</h2>
    <p style="margin:0 0 8px;font-size:14px;line-height:1.5;">{{.Plan.RootCause}}</p>
    {{if .Plan.Impact}}<p style="margin:0;font-size:14px;color:#57606a;">Impact: {{.Plan.Impact}}</p>{{end}}
    {{if .Verdict}}
      <p style="margin:8px 0 0;font-size:13px;color:{{if .Verdict.Approved}}#1a7f37{{else}}#9a6700{{end}};">
        Reviewer model: {{if .Verdict.Approved}}approved after {{.Incident.Attempts}} attempt(s){{else}}flagged concerns{{end}}
        {{range .Verdict.Issues}}<br>&bull; {{.}}{{end}}
      </p>
    {{end}}
  </section>

  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Proposed steps</h2>
    {{if .Steps}}
    <ol style="margin:0;padding-left:20px;font-size:14px;line-height:1.6;">
      {{range .Steps}}
      <li style="margin-bottom:10px;">
        <code style="display:block;background:#f6f8fa;border:1px solid #d0d7de;border-radius:6px;padding:8px;font-family:SFMono-Regular,Consolas,monospace;font-size:13px;white-space:pre-wrap;word-break:break-all;">{{.Run}}</code>
        {{if .Explain}}<span style="color:#57606a;">{{.Explain}}</span>{{end}}
        {{if .Risk}}<span style="color:#57606a;"> [{{.Risk}}]</span>{{end}}
      </li>
      {{end}}
    </ol>
    {{else}}
    <p style="margin:0;font-size:14px;">No executable step survived policy review. Treat this as a diagnosis only.</p>
    {{end}}
  </section>

  {{if .Blocked}}
  <section style="background:#fff8c5;border:1px solid #d4a72c;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Blocked by policy</h2>
    <ul style="margin:0;padding-left:20px;font-size:14px;line-height:1.6;">
      {{range .Blocked}}
      <li><code style="font-family:SFMono-Regular,Consolas,monospace;">{{.Run}}</code><br><span style="color:#57606a;">{{.Reason}}</span></li>
      {{end}}
    </ul>
  </section>
  {{end}}

  {{if .Plan.Verification}}
  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Verification</h2>
    <ul style="margin:0;padding-left:20px;font-size:14px;line-height:1.6;">{{range .Plan.Verification}}<li>{{.}}</li>{{end}}</ul>
    {{if .Plan.Rollback}}
    <h3 style="font-size:14px;margin:12px 0 4px;">Rollback</h3>
    <ul style="margin:0;padding-left:20px;font-size:14px;line-height:1.6;">{{range .Plan.Rollback}}<li>{{.}}</li>{{end}}</ul>
    {{end}}
  </section>
  {{end}}
  {{end}}

  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Your call</h2>
    <p style="margin:0 0 12px;font-size:14px;color:#57606a;">Approving stores this runbook and teaches retrieval for the next occurrence. Links expire.</p>
    <p style="margin:0;font-size:14px;">
      <a href="{{.Approve}}" style="display:inline-block;background:#1f883d;color:#ffffff;text-decoration:none;padding:10px 16px;border-radius:6px;margin-right:8px;">Approve this runbook</a>
      <a href="{{.Edit}}" style="display:inline-block;background:#0969da;color:#ffffff;text-decoration:none;padding:10px 16px;border-radius:6px;margin-right:8px;">Edit before approving</a>
      <a href="{{.Reject}}" style="display:inline-block;background:#cf222e;color:#ffffff;text-decoration:none;padding:10px 16px;border-radius:6px;">Reject</a>
    </p>
  </section>

  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Firing alerts</h2>
    <ul style="margin:0;padding-left:20px;font-size:14px;line-height:1.6;">
      {{range .Incident.Alerts}}<li><strong>{{.Name}}</strong> [{{.Severity}}] on {{.Target}}{{if .Summary}} &mdash; {{.Summary}}{{end}}</li>{{end}}
    </ul>
  </section>

  {{if .Matches}}
  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Historical runbooks used</h2>
    <ul style="margin:0;padding-left:20px;font-size:14px;line-height:1.6;">
      {{range .Matches}}<li>{{.Runbook.Title}} &middot; similarity {{pct .Score}}{{if .Reason}} &middot; {{.Reason}}{{end}}</li>{{end}}
    </ul>
  </section>
  {{end}}

  {{if .Events}}
  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Cluster events</h2>
    <pre style="margin:0;background:#f6f8fa;border:1px solid #d0d7de;border-radius:6px;padding:8px;font-size:12px;white-space:pre-wrap;">{{range .Events}}{{.String}}
{{end}}</pre>
  </section>
  {{end}}

  {{if .Logs}}
  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Log evidence</h2>
    <pre style="margin:0;background:#f6f8fa;border:1px solid #d0d7de;border-radius:6px;padding:8px;font-size:12px;white-space:pre-wrap;">{{range .Logs}}{{.String}}
{{end}}</pre>
  </section>
  {{end}}

  {{if .Trace}}
  <details style="font-size:12px;color:#57606a;">
    <summary style="cursor:pointer;">Pipeline trace</summary>
    <ul style="margin:8px 0 0;padding-left:20px;">{{range .Trace}}<li>{{.}}</li>{{end}}</ul>
  </details>
  {{end}}

  <p style="margin:16px 0 0;font-size:12px;color:#57606a;">
    Generated by WatchTower. Commands are suggestions produced from the evidence above. Review before running anything.
  </p>
</div>
</body>
</html>`

// digestTemplate is the daily executive summary.
const digestTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>WatchTower Daily Pulse</title>
</head>
<body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1b1f23;">
<div style="max-width:720px;margin:0 auto;padding:24px;">
  <h1 style="font-size:20px;margin:0 0 4px;">Daily Pulse</h1>
  <p style="margin:0 0 16px;color:#57606a;font-size:14px;">
    {{.From.Format "2006-01-02 15:04"}} to {{.To.Format "2006-01-02 15:04"}} UTC &middot;
    {{.Total}} reviewed &middot; {{.Approved}} approved &middot; {{.Edited}} edited &middot; {{.Rejected}} rejected
  </p>

  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;margin-bottom:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Summary</h2>
    <pre style="margin:0;font-family:inherit;font-size:14px;line-height:1.6;white-space:pre-wrap;">{{.Narrative}}</pre>
  </section>

  {{if .Entries}}
  <section style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:16px;">
    <h2 style="font-size:16px;margin:0 0 8px;">Reviewed incidents</h2>
    <ul style="margin:0;padding-left:20px;font-size:14px;line-height:1.6;">
      {{range .Entries}}
      <li>
        <strong>{{.Title}}</strong> [{{.Severity}}] &middot; {{.Decision}}{{if .Reviewer}} by {{.Reviewer}}{{end}}
        {{if .RootCause}}<br><span style="color:#57606a;">{{.RootCause}}</span>{{end}}
      </li>
      {{end}}
    </ul>
  </section>
  {{end}}

  <p style="margin:16px 0 0;font-size:12px;color:#57606a;">Generated by WatchTower from reviewed incidents only.</p>
</div>
</body>
</html>`
