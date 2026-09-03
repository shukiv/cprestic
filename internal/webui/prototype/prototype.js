/* PROTOTYPE — read-only UI exploration. Nothing here calls cprest. */
"use strict";

const variants = {
  A: "Operational rail",
  B: "Guided workspace",
  C: "Recovery console",
};

const pages = [
  ["overview", "Overview", "overview"],
  ["accounts", "Accounts", "accounts"],
  ["destinations", "Destinations", "database"],
  ["schedules", "Schedules", "calendar"],
  ["restore", "Restore", "restore"],
  ["history", "History", "history"],
  ["settings", "Settings", "settings"],
];

const pageMeta = {
  overview: ["Backup overview", "See what is protected, what needs attention, and what happened recently."],
  accounts: ["Accounts", "Protection state for every cPanel account on this server."],
  destinations: ["Destinations", "Storage endpoints, recovery keys, copy health, and retention."],
  schedules: ["Schedules", "Which accounts run, where copies go, and how long they are kept."],
  restore: ["Restore", "Choose a known-good copy, inspect it, then recover without surprises."],
  history: ["History", "Backups, restores, rehearsals, and cPanel lifecycle events."],
  settings: ["Settings", "Server behavior, lifecycle safeguards, staging, and notifications."],
};

const accounts = [
  { name: "northwind", domain: "northwind.example", size: "8.4 GB", last: "18 min ago", state: "Protected", tone: "good", copies: "Local vault · Wasabi EU", record: "28 / 28", action: "Open" },
  { name: "meridian", domain: "meridian-shop.example", size: "22.1 GB", last: "2 hr ago", state: "Copy missing", tone: "warn", copies: "Wasabi EU is stale", record: "26 / 27", action: "Repair copies" },
  { name: "atlasdemo", domain: "atlasdemo.example", size: "4.7 GB", last: "Never", state: "Not protected", tone: "bad", copies: "Daily schedule applies", record: "0 / 0", action: "Back up now" },
  { name: "cedar", domain: "cedar-studio.example", size: "1.9 GB", last: "Yesterday", state: "Protected", tone: "good", copies: "Local vault · Wasabi EU", record: "14 / 14", action: "Open" },
  { name: "lumen", domain: "lumen-labs.example", size: "12.8 GB", last: "Running · 64%", state: "Backing up", tone: "info", copies: "Local vault → Wasabi EU", record: "21 / 21", action: "View progress" },
];

const destinations = [
  { name: "Wasabi EU", kind: "S3 compatible", place: "eu-central-2", state: "Reachable", tone: "good", last: "Checked 4 min ago", stored: "184 GB", key: "Saved offline" },
  { name: "Local vault", kind: "Local disk", place: "/backup/cprest", state: "Reachable", tone: "good", last: "Checked 4 min ago", stored: "201 GB", key: "Saved offline" },
  { name: "Archive SFTP", kind: "SFTP", place: "backup-02.internal", state: "Needs attention", tone: "warn", last: "Last reached 3 days ago", stored: "96 GB", key: "Not confirmed" },
];

const schedules = [
  { name: "Daily complete", when: "Every day at 01:30", scope: "All 5 accounts", targets: "Local vault · Wasabi EU", keep: "7 daily · 5 weekly · 12 monthly", state: "On", tone: "good" },
  { name: "Weekly archive", when: "Sunday at 04:15", scope: "northwind · meridian", targets: "Archive SFTP", keep: "8 weekly · 12 monthly", state: "On", tone: "good" },
  { name: "Database quick copy", when: "Every 6 hours", scope: "All 5 accounts", targets: "Local vault", keep: "8 copies", state: "Partial payload", tone: "warn" },
];

const jobs = [
  { when: "Today 21:42", account: "northwind", kind: "Backup", result: "Succeeded", tone: "good", copy: "2 destinations", stored: "812 MB new", duration: "8m 14s" },
  { when: "Today 20:08", account: "meridian", kind: "Backup", result: "Partial", tone: "warn", copy: "1 of 2 destinations", stored: "1.4 GB new", duration: "19m 02s" },
  { when: "Today 18:30", account: "cedar", kind: "Rehearsal", result: "Passed", tone: "good", copy: "Wasabi EU", stored: "No live changes", duration: "4m 41s" },
  { when: "Yesterday 23:12", account: "atlasdemo", kind: "Backup", result: "Failed", tone: "bad", copy: "Staging", stored: "Nothing written", duration: "12s" },
];

const snapshots = {
  northwind: [
    { id: "40d8f2a1", label: "Today 21:42 · Wasabi EU · verified" },
    { id: "f1b902cc", label: "Yesterday 01:34 · Local vault" },
    { id: "7c84ea31", label: "Aug 31, 01:33 · Wasabi EU" },
  ],
  meridian: [
    { id: "a905ec44", label: "Today 20:08 · Local vault · partial copy repaired" },
    { id: "6ab2cc18", label: "Sep 2, 01:36 · Wasabi EU · verified" },
    { id: "42f6b701", label: "Aug 31, 01:39 · Local vault" },
  ],
  atlasdemo: [],
  cedar: [
    { id: "b231f983", label: "Yesterday 01:31 · Wasabi EU · verified" },
    { id: "08cc752a", label: "Sep 1, 01:32 · Local vault" },
  ],
  lumen: [
    { id: "c17f44b0", label: "Sep 2, 01:41 · Wasabi EU · verified" },
    { id: "02be0c7d", label: "Sep 1, 01:42 · Local vault" },
  ],
};

const restoreParts = [
  { key: "panel", label: "Panel Config", copy: "cPanel account settings, package, limits, and feature assignments." },
  { key: "home", label: "Home Dir", copy: "Files and folders under the account home directory." },
  { key: "cron", label: "Cron Jobs", copy: "The account’s scheduled commands and timing." },
  { key: "databases", label: "Databases", copy: "MySQL and MariaDB database contents." },
  { key: "db-users", label: "Database Users", copy: "Database users, grants, and account mappings." },
  { key: "domains", label: "Domains", copy: "Domains, subdomains, redirects, and DNS-related configuration." },
  { key: "certificates", label: "Certificates", copy: "SSL certificates, keys, and installed TLS configuration." },
  { key: "email", label: "Email", copy: "Mailboxes, forwarders, filters, and stored messages." },
  { key: "ftp", label: "FTP", copy: "FTP accounts, paths, and access configuration." },
];

const icons = {
  overview: '<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/>',
  accounts: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>',
  database: '<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v6c0 1.7 3.6 3 8 3s8-1.3 8-3V5"/><path d="M4 11v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/>',
  calendar: '<rect x="3" y="5" width="18" height="16" rx="2"/><path d="M16 3v4M8 3v4M3 11h18"/>',
  restore: '<path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l3 2"/>',
  history: '<path d="M3 3v5h5"/><path d="M3.1 13a9 9 0 1 0 2.1-6.4L3 8"/><path d="M12 7v5l4 2"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1-2.8 2.8-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.6v.2h-4V21a1.7 1.7 0 0 0-1-1.6 1.7 1.7 0 0 0-1.9.3l-.1.1L4.2 17l.1-.1a1.7 1.7 0 0 0 .3-1.9A1.7 1.7 0 0 0 3 14H2.8v-4H3a1.7 1.7 0 0 0 1.6-1 1.7 1.7 0 0 0-.3-1.9L4.2 7 7 4.2l.1.1A1.7 1.7 0 0 0 9 4.6 1.7 1.7 0 0 0 10 3V2.8h4V3a1.7 1.7 0 0 0 1 1.6 1.7 1.7 0 0 0 1.9-.3l.1-.1L19.8 7l-.1.1a1.7 1.7 0 0 0-.3 1.9 1.7 1.7 0 0 0 1.6 1h.2v4H21a1.7 1.7 0 0 0-1.6 1Z"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="m20 20-4-4"/>',
  shield: '<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z"/><path d="m9 12 2 2 4-4"/>',
  warning: '<path d="m21 19-9-16-9 16h18Z"/><path d="M12 9v4M12 17h.01"/>',
  check: '<path d="m20 6-11 11-5-5"/>',
  server: '<rect x="3" y="4" width="18" height="6" rx="2"/><rect x="3" y="14" width="18" height="6" rx="2"/><path d="M7 7h.01M7 17h.01"/>',
  bell: '<path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4"/>',
  arrow: '<path d="M5 12h14M13 6l6 6-6 6"/>',
  file: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8Z"/><path d="M14 2v6h6M8 13h8M8 17h6"/>',
  more: '<circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/>',
};

function icon(name) {
  return `<svg viewBox="0 0 24 24" aria-hidden="true">${icons[name] || icons.file}</svg>`;
}

function status(label, tone = "") {
  return `<span class="status ${tone}">${label}</span>`;
}

function action(label, kind = "", iconName = "") {
  return `<button type="button" class="btn ${kind}" data-action="${label}">${iconName ? icon(iconName) : ""}${label}</button>`;
}

function pageButton(key, label, iconName, className = "") {
  return `<button type="button" class="${className}" data-page="${key}" ${page === key ? 'aria-current="page"' : ""}>${icon(iconName)}<span>${label}</span></button>`;
}

function titleBlock(actionHTML = "") {
  const [title, copy] = pageMeta[page];
  return `<div class="a-page-head"><div class="title-block"><p class="eyebrow">mx.7171.online</p><h1>${title}</h1><p>${copy}</p></div><div class="button-row">${actionHTML}</div></div>`;
}

function accountRows(actions = true) {
  return accounts.map((account) => `
    <tr>
      <td><div class="primary-cell">${account.name}</div><div class="subcell">${account.domain}</div></td>
      <td class="num optional">${account.size}</td>
      <td>${status(account.state, account.tone)}<div class="subcell">${account.copies}</div></td>
      <td class="optional">${account.last}<div class="subcell">${account.record} successful</div></td>
      ${actions ? `<td class="right"><button type="button" class="btn compact ${account.tone === "bad" || account.tone === "warn" ? "primary" : ""}" data-action="${account.action} for ${account.name}">${account.action}</button></td>` : ""}
    </tr>`).join("");
}

function destinationRows() {
  return destinations.map((destination) => `
    <tr>
      <td><div class="primary-cell">${destination.name}</div><div class="subcell">${destination.kind} · ${destination.place}</div></td>
      <td>${status(destination.state, destination.tone)}<div class="subcell">${destination.last}</div></td>
      <td class="num optional">${destination.stored}</td>
      <td class="optional">${destination.key}</td>
      <td class="right"><button type="button" class="btn compact" data-action="Open ${destination.name}">Open</button></td>
    </tr>`).join("");
}

function scheduleRows() {
  return schedules.map((schedule) => `
    <tr>
      <td><div class="primary-cell">${schedule.name}</div><div class="subcell">${schedule.when}</div></td>
      <td>${schedule.scope}</td>
      <td class="optional">${schedule.targets}</td>
      <td class="optional">${schedule.keep}</td>
      <td>${status(schedule.state, schedule.tone)}</td>
      <td class="right"><button type="button" class="btn compact" data-action="Edit ${schedule.name}">Edit</button></td>
    </tr>`).join("");
}

function jobRows() {
  return jobs.map((job) => `
    <tr>
      <td class="num">${job.when}</td>
      <td><div class="primary-cell">${job.account}</div><div class="subcell">${job.kind}</div></td>
      <td>${status(job.result, job.tone)}</td>
      <td class="optional">${job.copy}</td>
      <td class="optional">${job.stored}<div class="subcell">${job.duration}</div></td>
      <td class="right"><button type="button" class="btn compact" data-action="View report for ${job.account}">Report</button></td>
    </tr>`).join("");
}

function table(headers, rows, label) {
  return `<div class="table-wrap"><table class="data-table"><caption class="visually-hidden">${label}</caption><thead><tr>${headers.map((header) => `<th class="${header.className || ""}">${header.label}</th>`).join("")}</tr></thead><tbody>${rows}</tbody></table></div>`;
}

function standardAccountsTable() {
  return table([
    { label: "Account" }, { label: "Size", className: "optional" }, { label: "Protection" },
    { label: "Latest", className: "optional" }, { label: "Action", className: "right" },
  ], accountRows(), "cPanel account protection");
}

function standardDestinationTable() {
  return table([
    { label: "Destination" }, { label: "Connection" }, { label: "Stored", className: "optional" },
    { label: "Recovery key", className: "optional" }, { label: "Action", className: "right" },
  ], destinationRows(), "Backup destinations");
}

function standardScheduleTable() {
  return table([
    { label: "Schedule" }, { label: "Accounts" }, { label: "Destinations", className: "optional" },
    { label: "Retention", className: "optional" }, { label: "State" }, { label: "Action", className: "right" },
  ], scheduleRows(), "Backup schedules");
}

function standardHistoryTable() {
  return table([
    { label: "When" }, { label: "Account" }, { label: "Result" }, { label: "Copies", className: "optional" },
    { label: "Storage", className: "optional" }, { label: "Action", className: "right" },
  ], jobRows(), "Recent cprest jobs");
}

function restoreProgress(step) {
  const labels = ["Choose restore type", "Configure restore", "Review and confirm"];
  return `<ol class="restore-progress" aria-label="Restore progress">${labels.map((label, index) => {
    const number = index + 1;
    const state = number < step ? "complete" : number === step ? "current" : "";
    return `<li class="${state}" ${number === step ? 'aria-current="step"' : ""}><span>${number < step ? icon("check") : number}</span><strong>${label}</strong></li>`;
  }).join("")}</ol>`;
}

function restoreChoice(direction) {
  return `<section class="restore-start restore-start-${direction}">
    ${restoreProgress(1)}
    <div class="restore-question"><p class="eyebrow">Start a restore</p><h2>How many accounts do you need to restore?</h2><p>Choose a path first. No files or cPanel accounts will be changed while you build the plan.</p></div>
    <div class="restore-choice-grid">
      <button type="button" class="restore-choice" data-restore-mode="single">
        <span class="restore-choice-icon">${icon("accounts")}</span>
        <span><strong>Restore one account</strong><small>Choose one cPanel account, a recovery date, and exactly which parts to restore.</small></span>
        ${icon("arrow")}
      </button>
      <button type="button" class="restore-choice" data-restore-mode="multiple">
        <span class="restore-choice-icon">${icon("restore")}</span>
        <span><strong>Restore multiple accounts</strong><small>Select several accounts—or all accounts—and choose a snapshot for each one.</small></span>
        ${icon("arrow")}
      </button>
    </div>
    <div class="notice info">${icon("shield")}<div><strong>Planning is read-only</strong><p>Every restore gets a final scope summary and confirmation before cPanel data can be changed.</p></div></div>
  </section>`;
}

function restoreModeHeader(title, copy) {
  return `<div class="restore-mode-head"><div><button type="button" class="btn ghost restore-back" data-restore-mode="">← Back to restore choices</button><h2>${title}</h2><p>${copy}</p></div>${restoreProgress(2)}</div>`;
}

function accountOptions() {
  return accounts.map((account) => {
    const available = snapshots[account.name].length > 0;
    return `<option value="${account.name}" ${singleAccount === account.name ? "selected" : ""} ${available ? "" : "disabled"}>${account.name} — ${account.domain}${available ? "" : " — no successful snapshot"}</option>`;
  }).join("");
}

function snapshotOptions(accountName, selectedId = "") {
  const available = snapshots[accountName] || [];
  if (!available.length) return '<option value="">No successful snapshot</option>';
  const resolved = available.some((snapshot) => snapshot.id === selectedId) ? selectedId : available[0].id;
  return available.map((snapshot) => `<option value="${snapshot.id}" ${snapshot.id === resolved ? "selected" : ""}>${snapshot.label}</option>`).join("");
}

function singleSourceFields(prefix) {
  const available = snapshots[singleAccount] || [];
  if (!available.some((snapshot) => snapshot.id === singleSnapshot)) singleSnapshot = available[0]?.id || "";
  return `<div class="grid-2">
    <div class="field"><label for="${prefix}-account">cPanel account</label><select id="${prefix}-account" data-single-account>${accountOptions()}</select><small>Accounts without a successful backup remain visible but cannot be selected.</small></div>
    <div class="field"><label for="${prefix}-snapshot">Restore date / snapshot</label><select id="${prefix}-snapshot" data-single-snapshot>${snapshotOptions(singleAccount, singleSnapshot)}</select><small>Choose the date whose contents should be restored. Verified snapshots appear first.</small></div>
  </div>`;
}

function restorePartsPicker(prefix) {
  return `<fieldset class="restore-parts"><legend>What should be restored?</legend><div class="restore-part-actions"><p>Select only what this account needs from the chosen date.</p><div class="button-row"><button type="button" class="btn compact" data-parts-action="all">Select all</button><button type="button" class="btn compact" data-parts-action="none">Clear</button></div></div><div class="restore-parts-grid">${restoreParts.map((part) => `<label class="restore-part" for="${prefix}-part-${part.key}"><input id="${prefix}-part-${part.key}" type="checkbox" data-restore-part="${part.key}" ${selectedRestoreParts.has(part.key) ? "checked" : ""}><span><strong>${part.label}</strong><small>${part.copy}</small></span></label>`).join("")}</div></fieldset>`;
}

function singleOutcomeField(prefix) {
  return `<div class="field"><label for="${prefix}-outcome">Restore outcome</label><select id="${prefix}-outcome"><option>Rebuild an archive for review and download</option><option>Run a recovery rehearsal only</option><option>Apply selected parts to the live cPanel account</option></select><small>Live changes require a separate confirmation on the review step.</small></div>`;
}

function selectedPartLabels() {
  return restoreParts.filter((part) => selectedRestoreParts.has(part.key)).map((part) => part.label);
}

function singlePlanSummary() {
  const chosen = selectedPartLabels();
  const selected = snapshots[singleAccount]?.find((snapshot) => snapshot.id === singleSnapshot) || snapshots[singleAccount]?.[0];
  return `<div class="restore-summary" aria-live="polite"><div><span>Account</span><strong>${singleAccount}</strong></div><div><span>Recovery date</span><strong>${selected?.label || "No snapshot selected"}</strong></div><div><span>Contents</span><strong data-single-part-count>${chosen.length} of ${restoreParts.length} selected</strong><small data-single-part-list>${chosen.length ? chosen.join(" · ") : "Choose at least one item"}</small></div></div>`;
}

function singleReviewButton(label = "Review restore") {
  return `<button type="button" class="btn primary" data-action="${label}" data-single-review ${selectedRestoreParts.size ? "" : "disabled"}>${icon("arrow")}${label}</button>`;
}

function bulkSelectedAccountNames() {
  const availableNames = accounts.filter((account) => snapshots[account.name].length).map((account) => account.name);
  return bulkScope === "all" ? availableNames : availableNames.filter((name) => selectedRestoreAccounts.has(name));
}

function bulkScopeControls() {
  return `<fieldset class="bulk-scope"><legend>Account scope</legend><div class="bulk-scope-options"><button type="button" data-bulk-scope="selected" aria-pressed="${bulkScope === "selected"}"><strong>Choose accounts</strong><small>Select only the accounts required for this recovery.</small></button><button type="button" data-bulk-scope="all" aria-pressed="${bulkScope === "all"}"><strong>Restore all accounts</strong><small>Include every account that has a successful snapshot.</small></button></div></fieldset>`;
}

function bulkAccountRows(prefix) {
  const chosenNames = new Set(bulkSelectedAccountNames());
  return `<div class="bulk-account-list">${accounts.map((account) => {
    const available = snapshots[account.name].length > 0;
    const selected = available && chosenNames.has(account.name);
    const savedSnapshot = bulkSnapshots.get(account.name) || snapshots[account.name][0]?.id || "";
    return `<article class="bulk-account-row ${selected ? "selected" : ""} ${available ? "" : "unavailable"}" data-bulk-row="${account.name}">
      <label class="bulk-account-toggle" for="${prefix}-bulk-${account.name}"><input id="${prefix}-bulk-${account.name}" type="checkbox" data-bulk-account="${account.name}" ${selected ? "checked" : ""} ${!available || bulkScope === "all" ? "disabled" : ""}><span><strong>${account.name}</strong><small>${account.domain} · ${account.size}</small></span></label>
      <div>${available ? status(selected ? "Included" : "Not selected", selected ? "good" : "") : status("No successful snapshot", "bad")}</div>
      <div class="field"><label for="${prefix}-bulk-snapshot-${account.name}">Snapshot to restore</label><select id="${prefix}-bulk-snapshot-${account.name}" data-bulk-snapshot="${account.name}" ${selected ? "" : "disabled"}>${snapshotOptions(account.name, savedSnapshot)}</select></div>
    </article>`;
  }).join("")}</div>`;
}

function bulkUnavailableNotice() {
  if (bulkScope !== "all") return "";
  return `<div class="notice">${icon("warning")}<div><strong>atlasdemo cannot be included</strong><p>It has no successful snapshot. The restore plan includes the other four accounts and clearly records atlasdemo as skipped.</p></div></div>`;
}

function bulkPlanSummary() {
  const selected = bulkSelectedAccountNames();
  const suffix = bulkScope === "all" ? " · 1 unavailable" : "";
  return `<div class="restore-summary" aria-live="polite"><div><span>Account scope</span><strong>${bulkScope === "all" ? "All restorable accounts" : "Selected accounts"}</strong></div><div><span>Ready</span><strong data-bulk-count>${selected.length} account${selected.length === 1 ? "" : "s"}${suffix}</strong></div><div><span>Payload</span><strong>Complete cPanel account</strong><small>Each row uses its explicitly selected snapshot.</small></div></div>`;
}

function bulkReviewButton(label = "Review bulk restore") {
  return `<button type="button" class="btn primary" data-action="${label}" data-bulk-review ${bulkSelectedAccountNames().length ? "" : "disabled"}>${icon("arrow")}${label}</button>`;
}

function overviewA() {
  return `${titleBlock(action("Back up all", "primary", "restore"))}
    <div class="stack-lg">
      <div class="notice">${icon("warning")}<div><strong>Two accounts need action</strong><p>meridian is missing its Wasabi copy; atlasdemo has never completed a backup.</p></div></div>
      <section class="a-kpis" aria-label="Protection summary">
        <div class="card a-health"><div class="a-health-top"><div><div class="eyebrow">Coverage</div><div class="a-score">73%</div></div>${status("3 of 5 protected", "good")}</div><div class="meter" aria-label="3 protected, 1 stale, 1 unprotected"><span></span><span></span><span></span></div><div class="muted">Every promised destination is counted separately.</div></div>
        <div class="card metric"><div class="label">Protected</div><div class="value">3</div><div class="detail">Current complete copies</div></div>
        <div class="card metric"><div class="label">Needs action</div><div class="value">2</div><div class="detail">1 stale · 1 never</div></div>
        <div class="card metric"><div class="label">Last rehearsal</div><div class="value">Passed</div><div class="detail">cedar · today 18:30</div></div>
      </section>
      <div class="a-two">
        <section class="card"><div class="card-head"><div><h2 class="section-title">Act now</h2><p class="section-copy">Exceptions are ordered by risk.</p></div><button class="btn compact" data-page="accounts">All accounts</button></div><div class="card-pad task-list">
          <div class="task">${status("Copy missing", "warn")}<div><div class="activity-title">meridian</div><div class="activity-copy">Wasabi EU has no recent successful copy</div></div><button class="btn compact primary" data-action="Repair meridian copies">Repair copies</button></div>
          <div class="task">${status("Never backed up", "bad")}<div><div class="activity-title">atlasdemo</div><div class="activity-copy">Daily complete applies but has never succeeded</div></div><button class="btn compact primary" data-action="Back up atlasdemo">Back up now</button></div>
        </div></section>
        <section class="card"><div class="card-head"><div><h2 class="section-title">Recent activity</h2><p class="section-copy">Last lifecycle and backup changes.</p></div></div><div class="card-pad activity-list">
          <div class="activity"><span class="activity-icon">${icon("check")}</span><div><div class="activity-title">northwind backed up</div><div class="activity-copy">2 copies · 18 min ago</div></div>${status("Succeeded", "good")}</div>
          <div class="activity"><span class="activity-icon">${icon("shield")}</span><div><div class="activity-title">lumen suspended</div><div class="activity-copy">Preservation backup queued</div></div>${status("Handled", "info")}</div>
          <div class="activity"><span class="activity-icon">${icon("restore")}</span><div><div class="activity-title">cedar rehearsal</div><div class="activity-copy">Verified restore · 3 hr ago</div></div>${status("Passed", "good")}</div>
        </div></section>
      </div>
    </div>`;
}

function accountsA() {
  return `${titleBlock(action("Back up all", "primary", "restore"))}
    <div class="stack">
      <div class="spread"><div class="tabs"><button class="tab" aria-pressed="true">All 5</button><button class="tab" aria-pressed="false">Needs action 2</button><button class="tab" aria-pressed="false">Running 1</button></div><label class="search">${icon("search")}<span class="visually-hidden">Search accounts</span><input type="search" placeholder="Search accounts"></label></div>
      <section class="card">${standardAccountsTable()}</section>
    </div>`;
}

function destinationsA() {
  return `${titleBlock(action("Add destination", "primary", "database"))}
    <div class="stack">
      <div class="notice">${icon("warning")}<div><strong>Archive SFTP needs attention</strong><p>It has been unreachable for three days and its recovery-key copy is not confirmed.</p></div></div>
      <section class="card">${standardDestinationTable()}</section>
      <section class="grid-3">
        <div class="card card-pad metric"><div class="label">Physical storage</div><div class="value">481 GB</div><div class="detail">Across three destinations</div></div>
        <div class="card card-pad metric"><div class="label">Recovery keys</div><div class="value">2 / 3</div><div class="detail">Confirmed outside this server</div></div>
        <div class="card card-pad metric"><div class="label">Next retention</div><div class="value">Sun</div><div class="detail">Plan first, then approve</div></div>
      </section>
    </div>`;
}

function schedulesA() {
  return `${titleBlock(action("New schedule", "primary", "calendar"))}<section class="card">${standardScheduleTable()}</section>`;
}

function restoreA() {
  if (!restoreMode) return `${titleBlock()}${restoreChoice("operational")}`;
  if (restoreMode === "single") {
    return `${titleBlock()}${restoreModeHeader("Restore one account", "Choose the account, the recovery date, and every cPanel component that should be recovered.")}
      <div class="a-two restore-builder">
        <section class="card"><div class="card-head"><div><h3 class="section-title">Build the restore plan</h3><p class="section-copy">The latest verified snapshot is selected by default.</p></div>${status("Step 2 of 3", "info")}</div><div class="card-pad stack-lg">
          ${singleSourceFields("a-single")}
          ${restorePartsPicker("a-single")}
          ${singleOutcomeField("a-single")}
          <div class="notice info">${icon("shield")}<div><strong>Safe default</strong><p>The selected data is rebuilt for review and download. Nothing on the live account is overwritten.</p></div></div>
          <div class="button-row">${singleReviewButton()} ${action("Run rehearsal", "", "shield")}</div>
        </div></section>
        <aside class="card restore-sticky"><div class="card-head"><div><h3 class="section-title">Plan summary</h3><p class="section-copy">Exactly what will move forward to review.</p></div></div><div class="card-pad stack">${singlePlanSummary()}<div class="spread"><span>Snapshot evidence</span>${status("Verified", "good")}</div><div class="spread"><span>Live changes</span><strong>None yet</strong></div></div></aside>
      </div>`;
  }
  return `${titleBlock()}${restoreModeHeader("Restore multiple accounts", "Choose specific accounts or include every restorable account, then select a snapshot for each one.")}
    <div class="a-two restore-builder">
      <section class="card"><div class="card-head"><div><h3 class="section-title">Accounts and snapshots</h3><p class="section-copy">Every included account must have an explicit recovery point.</p></div>${status("Step 2 of 3", "info")}</div><div class="card-pad stack-lg">${bulkScopeControls()}${bulkUnavailableNotice()}${bulkAccountRows("a")}</div></section>
      <aside class="card restore-sticky"><div class="card-head"><div><h3 class="section-title">Bulk plan</h3><p class="section-copy">Complete-account restores, queued only after review.</p></div></div><div class="card-pad stack">${bulkPlanSummary()}<div class="notice info">${icon("shield")}<div><strong>One final checkpoint</strong><p>Review account names and snapshots before any live cPanel restore can start.</p></div></div><div class="button-row">${bulkReviewButton()}</div></div></aside>
    </div>`;
}

function historyA() {
  return `${titleBlock(action("Export view", "", "file"))}
    <div class="stack"><div class="tabs"><button class="tab" aria-pressed="true">All activity</button><button class="tab" aria-pressed="false">Backups</button><button class="tab" aria-pressed="false">Restores</button><button class="tab" aria-pressed="false">Lifecycle</button></div><section class="card">${standardHistoryTable()}</section></div>`;
}

function settingsA() {
  return `${titleBlock(action("Save changes", "primary", "check"))}
    <div class="a-two"><div class="stack">
      <section class="card"><div class="card-head"><div><h2 class="section-title">Backup engine</h2><p class="section-copy">Paths and limits used for every run.</p></div></div><div class="card-pad grid-2"><div class="field"><label for="a-host">Server name</label><input id="a-host" value="mx.7171.online"><small>Written into every snapshot.</small></div><div class="field"><label for="a-concurrency">Accounts at once</label><input id="a-concurrency" type="number" value="1"><small>One account can consume its full staged size.</small></div><div class="field"><label for="a-restic">restic binary</label><input id="a-restic" value="/usr/local/bin/restic"></div><div class="field"><label for="a-output">Keep restored files</label><select id="a-output"><option>7 days</option><option>14 days</option><option>Until deleted</option></select></div></div></section>
      <section class="card"><div class="card-head"><div><h2 class="section-title">cPanel lifecycle safeguards</h2><p class="section-copy">Automation around suspension and termination.</p></div></div><div class="card-pad"><label class="check-row"><input type="checkbox"><span><span class="check-title">Block unsafe account termination</span><span class="check-copy">Require recent complete copies at every promised destination before cPanel removes an account.</span></span></label><label class="check-row"><input type="checkbox"><span><span class="check-title">Preserve accounts when suspended</span><span class="check-copy">Queue full-account copies when cPanel reports a suspension.</span></span></label></div></section>
    </div><aside class="stack"><section class="card card-pad"><p class="eyebrow">Staging</p><div class="metric"><div class="value">6.2 GB</div><div class="detail">Free at /var/lib/cprest/staging</div></div></section><section class="card card-pad"><p class="eyebrow">Notifications</p><div class="spread"><div><strong>Operations email</strong><div class="subcell">Failures and overdue copies</div></div>${status("Working", "good")}</div><div class="button-row" style="margin-top:16px">${action("Send test")}</div></section></aside></div>`;
}

function pageA() {
  return ({ overview: overviewA, accounts: accountsA, destinations: destinationsA, schedules: schedulesA, restore: restoreA, history: historyA, settings: settingsA }[page] || overviewA)();
}

function renderA() {
  return `<div class="variant-a"><div class="a-shell"><aside class="a-rail"><div class="brand"><span class="brand-mark">cP</span><span class="brand-name">cP:Restic</span></div><div class="a-server">mx.7171.online</div><nav class="a-nav" aria-label="cP:Restic pages">${pages.map(([key, label, iconName]) => pageButton(key, label, iconName)).join("")}</nav><div class="a-rail-foot"><span>${status("Service healthy", "good")}</span><span>cPanel 136 · root</span></div></aside><div class="a-main"><header class="a-topbar"><span class="a-breadcrumb">WHM / Plugins / cP:Restic</span><div class="button-row"><button type="button" class="icon-btn" data-action="Open notifications" aria-label="Open notifications">${icon("bell")}</button><button type="button" class="btn compact" data-action="Open server menu">root@mx.7171.online</button></div></header><main id="prototype-main" class="a-content" tabindex="-1">${pageA()}</main></div></div></div>`;
}

function guidedHeader(primaryAction = "") {
  const [title, copy] = pageMeta[page];
  return `<div class="b-section-head"><div class="title-block"><p class="eyebrow">${page === "overview" ? "Good evening" : "cP:Restic"}</p><h1>${title}</h1><p>${copy}</p></div>${primaryAction ? `<div>${primaryAction}</div>` : ""}</div>`;
}

function overviewB() {
  return `<section class="b-hero"><div><p class="eyebrow">Good evening</p><h1>Your backups are mostly healthy.</h1><p>Three accounts have complete, current copies everywhere promised. Two clear tasks remain before this server is fully protected.</p><div class="button-row">${action("Fix the two issues", "primary", "arrow")} ${action("Review all accounts", "", "accounts")}</div></div><div class="b-score-card"><div class="eyebrow" style="color:#b9cee6">Protection score</div><div class="score">73%</div><p>3 protected · 1 stale copy · 1 never backed up</p><div class="meter" aria-label="3 protected, 1 stale, 1 unprotected"><span></span><span></span><span></span></div></div></section>
    <section class="b-steps" aria-label="Recommended next steps">
      <article class="card b-step"><span class="b-step-number">1</span><h2>Repair meridian</h2><p>Refresh its missing Wasabi EU copy using Daily complete.</p>${action("Repair copy", "primary")}</article>
      <article class="card b-step"><span class="b-step-number">2</span><h2>Protect atlasdemo</h2><p>It is on the schedule but has never completed a first backup.</p>${action("Back up now", "primary")}</article>
      <article class="card b-step"><span class="b-step-number">3</span><h2>Confirm recovery key</h2><p>Record that Archive SFTP’s key is stored away from this server.</p>${action("Review key")}</article>
    </section>
    <section class="b-section"><div class="b-section-head"><div><h2 class="section-title">What happened recently</h2><p class="section-copy">Important changes, written in plain language.</p></div><button type="button" class="btn compact" data-page="history">See complete history</button></div><div class="b-list">
      <article class="card b-list-row"><span class="b-list-icon">${icon("check")}</span><div><h3>northwind now has two current copies</h3><p>Local vault and Wasabi EU completed 18 minutes ago.</p></div>${status("Protected", "good")}</article>
      <article class="card b-list-row"><span class="b-list-icon">${icon("shield")}</span><div><h3>lumen was suspended in cPanel</h3><p>The lifecycle event was handled and preservation work was queued.</p></div>${status("Handled", "info")}</article>
      <article class="card b-list-row"><span class="b-list-icon">${icon("restore")}</span><div><h3>cedar passed a recovery rehearsal</h3><p>The account archive rebuilt without changing its live files.</p></div>${status("Verified", "good")}</article>
    </div></section>`;
}

function accountsB() {
  return `${guidedHeader(action("Back up all accounts", "primary", "restore"))}<div class="spread" style="margin:22px 0 14px"><div class="tabs"><button class="tab" aria-pressed="true">All 5</button><button class="tab" aria-pressed="false">Needs help 2</button><button class="tab" aria-pressed="false">Protected 3</button></div><label class="search">${icon("search")}<span class="visually-hidden">Search accounts</span><input type="search" placeholder="Find an account"></label></div><section class="b-account-grid">${accounts.map((account) => `<article class="card b-account"><span class="avatar">${account.name.slice(0, 2)}</span><div><h3>${account.name}</h3><p>${account.domain} · ${account.size}</p>${status(account.state, account.tone)}<div class="subcell" style="margin-top:8px">${account.copies}<br>Latest: ${account.last}</div></div><button type="button" class="btn compact ${account.tone === "warn" || account.tone === "bad" ? "primary" : ""}" data-action="${account.action} for ${account.name}">${account.action}</button></article>`).join("")}</section>`;
}

function destinationsB() {
  return `${guidedHeader(action("Connect storage", "primary", "database"))}<div class="notice" style="margin:22px 0">${icon("warning")}<div><strong>One destination needs a decision</strong><p>Archive SFTP has been offline for three days. Retry it, update its connection, or remove it from the weekly promise.</p></div></div><section class="b-list">${destinations.map((destination, index) => `<article class="card b-list-row"><span class="b-list-icon">${icon(index === 1 ? "server" : "database")}</span><div><h3>${destination.name}</h3><p>${destination.kind} · ${destination.place} · ${destination.stored} stored</p><div class="button-row" style="margin-top:10px">${status(destination.state, destination.tone)} ${status(destination.key, destination.key.includes("Not") ? "warn" : "good")}</div></div><button type="button" class="btn" data-action="Manage ${destination.name}">Manage</button></article>`).join("")}</section><section class="b-section card card-pad"><div class="grid-3"><div class="metric"><div class="label">Copies promised</div><div class="value">8</div><div class="detail">Across five accounts</div></div><div class="metric"><div class="label">Copies current</div><div class="value">7</div><div class="detail">One Wasabi gap</div></div><div class="metric"><div class="label">Keys safe offline</div><div class="value">2 / 3</div><div class="detail">One confirmation needed</div></div></div></section>`;
}

function schedulesB() {
  return `${guidedHeader(action("Create a schedule", "primary", "calendar"))}<div class="b-section b-list">${schedules.map((schedule, index) => `<article class="card b-list-row"><span class="b-step-number">${index + 1}</span><div><h3>${schedule.name}</h3><p>${schedule.when} · ${schedule.scope}</p><div class="subcell" style="margin-top:8px"><strong>Writes to:</strong> ${schedule.targets}<br><strong>Keeps:</strong> ${schedule.keep}</div></div><div class="stack" style="justify-items:end">${status(schedule.state, schedule.tone)}<button type="button" class="btn compact" data-action="Edit ${schedule.name}">Edit schedule</button></div></article>`).join("")}</div><section class="b-section notice info">${icon("shield")}<div><strong>Why separate schedules?</strong><p>Each schedule makes an explicit promise about payload, destination, frequency, and retention. Coverage is healthy only when every promise is current.</p></div></section>`;
}

function restoreB() {
  if (!restoreMode) return `${guidedHeader()}${restoreChoice("guided")}`;
  if (restoreMode === "single") {
    return `${guidedHeader()}${restoreModeHeader("Restore one account", "Work from source to scope. You can change any choice before review.")}
      <section class="b-restore-steps" aria-label="Configure a single-account restore">
        <article class="card b-restore-step"><span class="b-step-number">1</span><div><h3>Choose the account and date</h3><p>The date determines which version of every selected component is recovered.</p></div>${singleSourceFields("b-single")}</article>
        <article class="card b-restore-step"><span class="b-step-number">2</span><div><h3>Choose what to restore</h3><p>Select all nine components for a complete account, or only the parts that need recovery.</p></div>${restorePartsPicker("b-single")}</article>
        <article class="card b-restore-step"><span class="b-step-number">3</span><div><h3>Choose the outcome</h3><p>Reviewable output is the default; applying to cPanel needs an extra confirmation.</p></div>${singleOutcomeField("b-single")}</article>
      </section>
      <section class="b-section card card-pad"><div class="guided-review"><div><h3 class="section-title">Your restore plan</h3>${singlePlanSummary()}</div><div class="stack"><span class="muted">Step 3 is a read-only review.</span>${singleReviewButton("Continue to review")}</div></div></section>`;
  }
  return `${guidedHeader()}${restoreModeHeader("Restore multiple accounts", "Start with the account scope, then confirm one recovery snapshot for each selected account.")}
    <section class="card card-pad b-bulk-scope"><div><p class="eyebrow">1 · Choose scope</p><h3>Which accounts should be restored?</h3><p class="section-copy">Pick individual accounts or include every account that has a successful backup.</p></div>${bulkScopeControls()}</section>
    <section class="b-section"><div class="b-section-head"><div><p class="eyebrow">2 · Choose recovery points</p><h3 class="section-title">One snapshot per account</h3><p class="section-copy">The most recent usable snapshot is preselected and can be changed per row.</p></div><span data-bulk-count>${bulkSelectedAccountNames().length} accounts ready</span></div>${bulkUnavailableNotice()}${bulkAccountRows("b")}</section>
    <section class="b-section card card-pad"><div class="guided-review"><div><p class="eyebrow">3 · Review</p><h3 class="section-title">Ready for the final checkpoint</h3>${bulkPlanSummary()}</div><div class="stack"><span class="muted">No cPanel changes have been made.</span>${bulkReviewButton("Continue to review")}</div></div></section>`;
}

function historyB() {
  return `${guidedHeader(action("Export activity", "", "file"))}<div class="tabs" style="margin:22px 0 14px"><button class="tab" aria-pressed="true">Everything</button><button class="tab" aria-pressed="false">Needs attention</button><button class="tab" aria-pressed="false">Lifecycle</button></div><section class="b-list">${jobs.map((job) => `<article class="card b-list-row"><span class="b-list-icon">${icon(job.kind === "Rehearsal" ? "restore" : "history")}</span><div><h3>${job.account} · ${job.kind.toLowerCase()}</h3><p>${job.when} · ${job.copy} · ${job.stored} · ${job.duration}</p></div><div class="stack" style="justify-items:end">${status(job.result, job.tone)}<button type="button" class="btn compact" data-action="View ${job.account} report">View report</button></div></article>`).join("")}</section>`;
}

function settingsB() {
  return `${guidedHeader(action("Save changes", "primary", "check"))}<div class="b-settings" style="margin-top:24px"><nav class="card b-settings-nav" aria-label="Settings sections"><button type="button">Backup engine</button><button type="button">Lifecycle safeguards</button><button type="button">Notifications</button><button type="button">Staging and output</button><button type="button">Advanced</button></nav><div class="stack-lg"><section class="card"><div class="card-head"><div><h2 class="section-title">Backup engine</h2><p class="section-copy">The everyday settings for this server.</p></div></div><div class="card-pad grid-2"><div class="field"><label for="b-host">Server name</label><input id="b-host" value="mx.7171.online"><small>Recorded on each backup.</small></div><div class="field"><label for="b-concurrency">Accounts at once</label><input id="b-concurrency" type="number" value="1"><small>Lower is safer for staging space.</small></div><div class="field"><label for="b-retain">Keep restored files</label><select id="b-retain"><option>7 days</option><option>14 days</option><option>Until deleted</option></select></div><div class="field"><label for="b-binary">restic binary</label><input id="b-binary" value="/usr/local/bin/restic"></div></div></section><section class="card"><div class="card-head"><div><h2 class="section-title">Lifecycle safeguards</h2><p class="section-copy">Extra protection around cPanel account changes.</p></div></div><div class="card-pad"><label class="check-row"><input type="checkbox"><span><span class="check-title">Block unsafe termination</span><span class="check-copy">cPanel cannot remove an account until every complete-copy promise is current.</span></span></label><label class="check-row"><input type="checkbox"><span><span class="check-title">Back up when suspended</span><span class="check-copy">Queue preservation without holding up the cPanel suspension request.</span></span></label></div></section></div></div>`;
}

function pageB() {
  return ({ overview: overviewB, accounts: accountsB, destinations: destinationsB, schedules: schedulesB, restore: restoreB, history: historyB, settings: settingsB }[page] || overviewB)();
}

function renderB() {
  return `<div class="variant-b"><header class="b-top"><div class="b-top-inner"><div class="brand"><span class="brand-mark">cP</span><span>cP:Restic</span></div><nav class="b-nav" aria-label="cP:Restic pages">${pages.map(([key, label]) => `<button type="button" data-page="${key}" ${page === key ? 'aria-current="page"' : ""}>${label}</button>`).join("")}</nav><button type="button" class="icon-btn" data-action="Open notifications" aria-label="Open notifications">${icon("bell")}</button></div></header><div class="b-context"><div class="b-context-inner"><span><strong>mx.7171.online</strong> · cPanel 136 · standalone node</span>${status("Service healthy", "good")}</div></div><main id="prototype-main" class="b-content" tabindex="-1">${pageB()}</main></div>`;
}

function consoleSummary() {
  return `<section class="c-summary" aria-label="System summary"><div class="metric"><div class="label">Coverage</div><div class="value">73%</div></div><div class="metric"><div class="label">Protected</div><div class="value">3 / 5</div></div><div class="metric"><div class="label">Copy gaps</div><div class="value">1</div></div><div class="metric"><div class="label">Running</div><div class="value">1</div></div><div class="metric"><div class="label">Destinations</div><div class="value">2 / 3 up</div></div></section>`;
}

function consolePagebar(actionHTML = "") {
  const [title, copy] = pageMeta[page];
  return `<header class="c-pagebar"><div><h1>${title}</h1><p>${copy}</p></div><div class="button-row">${actionHTML}</div></header>`;
}

function overviewC() {
  return `${consolePagebar(action("Queue backup", "primary", "restore"))}${consoleSummary()}<section class="c-panel"><div class="c-panel-head"><h2>Protection exceptions</h2><div class="tabs"><button class="tab" aria-pressed="true">Risk order</button><button class="tab" aria-pressed="false">Account</button></div></div><div class="c-master-detail"><div class="c-master">${standardAccountsTable()}</div><aside class="c-detail"><p class="eyebrow">Selected exception</p><h2>meridian</h2><p class="muted">meridian-shop.example</p>${status("Copy missing", "warn")}<h3>Why it is here</h3><p>Wasabi EU has no successful complete copy inside the Daily complete freshness window.</p><dl><dt>Latest local copy</dt><dd>Today 20:08</dd><dt>Latest Wasabi copy</dt><dd>3 days ago</dd><dt>Termination</dt><dd>Would be blocked</dd><dt>Repair policy</dt><dd>Daily complete</dd></dl><div class="button-row">${action("Repair copies", "primary")} ${action("Open account")}</div></aside></div></section>`;
}

function accountsC() {
  return `${consolePagebar(action("Queue backup", "primary", "restore"))}${consoleSummary()}<section class="c-panel"><div class="c-panel-head"><label class="search" style="max-width:360px">${icon("search")}<span class="visually-hidden">Filter accounts</span><input type="search" placeholder="Filter accounts"></label><span class="muted">5 rows · sorted by risk</span></div><div class="c-master-detail"><div class="c-master">${standardAccountsTable()}</div><aside class="c-detail"><p class="eyebrow">Account detail</p><h2>meridian</h2><p class="muted">22.1 GB · uid 1048</p>${status("Copy missing", "warn")}<h3>Copy matrix</h3><dl><dt>Local vault</dt><dd>${status("Current", "good")}</dd><dt>Wasabi EU</dt><dd>${status("Stale", "warn")}</dd><dt>Archive SFTP</dt><dd>${status("Not promised")}</dd></dl><h3>Evidence</h3><dl><dt>Success record</dt><dd>26 / 27</dd><dt>Last rehearsal</dt><dd>Never</dd><dt>Safe to terminate</dt><dd>No</dd></dl><div class="button-row">${action("Repair copies", "primary")} ${action("Verify restore")}</div></aside></div></section>`;
}

function destinationsC() {
  return `${consolePagebar(action("Add destination", "primary", "database"))}<section class="c-panel"><div class="c-panel-head"><h2>Repository endpoints</h2><span class="muted">3 configured · 1 degraded</span></div><div class="c-master-detail"><div class="c-master">${standardDestinationTable()}</div><aside class="c-detail"><p class="eyebrow">Selected destination</p><h2>Archive SFTP</h2><p class="muted mono">backup-02.internal:/srv/restic</p>${status("Needs attention", "warn")}<h3>Connection</h3><dl><dt>Last success</dt><dd>3 days ago</dd><dt>Credential</dt><dd>SSH key</dd><dt>Append-only</dt><dd>Enabled</dd><dt>Stored</dt><dd>96 GB</dd></dl><h3>Recovery</h3><p>The repository key has not been confirmed outside this server.</p><div class="button-row">${action("Test now", "primary")} ${action("Edit endpoint")}</div></aside></div></section>`;
}

function schedulesC() {
  return `${consolePagebar(action("New schedule", "primary", "calendar"))}<section class="c-panel"><div class="c-panel-head"><h2>Policy matrix</h2><span class="muted">3 schedules · 8 copy promises</span></div><div class="c-master-detail"><div class="c-master">${standardScheduleTable()}</div><aside class="c-detail"><p class="eyebrow">Selected schedule</p><h2>Daily complete</h2><p class="muted mono">30 1 * * *</p>${status("Enabled", "good")}<h3>Coverage</h3><dl><dt>Accounts</dt><dd>All 5</dd><dt>Payload</dt><dd>Complete</dd><dt>Destinations</dt><dd>2</dd><dt>Next run</dt><dd>01:30</dd></dl><h3>Retention</h3><p>7 daily · 5 weekly · 12 monthly snapshots.</p><div class="button-row">${action("Run now", "primary")} ${action("Edit policy")}</div></aside></div></section>`;
}

function restoreC() {
  if (!restoreMode) return `${consolePagebar()}<section class="c-panel c-restore-start"><div class="c-panel-head"><h2>New recovery plan</h2><span class="muted">Step 1 of 3 · read-only</span></div>${restoreChoice("console")}</section>`;
  if (restoreMode === "single") {
    return `${consolePagebar()}${restoreModeHeader("Single-account recovery", "Select the source date and construct an exact cPanel payload.")}
      <section class="c-panel"><div class="c-panel-head"><h2>Recovery builder / single account</h2><span class="muted">Step 2 of 3 · no live writes</span></div><div class="c-master-detail"><div class="c-master"><div class="card-pad stack-lg">${singleSourceFields("c-single")}${restorePartsPicker("c-single")}${singleOutcomeField("c-single")}<div class="button-row">${singleReviewButton("Review recovery plan")} ${action("Run rehearsal", "", "shield")}</div></div></div><aside class="c-detail"><p class="eyebrow">Plan preview</p><h2>${singleAccount}</h2>${status("Ready", "good")}<h3>Source and contents</h3>${singlePlanSummary()}<h3>Guardrail</h3><p>The review step lists each selected component before <span class="mono">restorepkg</span> or a component-level restore can run.</p></aside></div></section>`;
  }
  return `${consolePagebar()}${restoreModeHeader("Multi-account recovery", "Select accounts, bind each one to a snapshot, then inspect the complete restore matrix.")}
    <section class="c-panel"><div class="c-panel-head"><h2>Recovery matrix / multiple accounts</h2><span class="muted">Step 2 of 3 · no live writes</span></div><div class="c-master-detail"><div class="c-master"><div class="card-pad stack-lg">${bulkScopeControls()}${bulkUnavailableNotice()}${bulkAccountRows("c")}</div></div><aside class="c-detail"><p class="eyebrow">Queue preview</p><h2>${bulkSelectedAccountNames().length} accounts</h2>${status("Ready for review", "good")}<h3>Plan</h3>${bulkPlanSummary()}<h3>Execution</h3><p>Accounts are restored one at a time. A failure pauses the queue before the next live account is changed.</p><div class="button-row">${bulkReviewButton("Review recovery matrix")}</div></aside></div></section>`;
}

function historyC() {
  return `${consolePagebar(action("Export", "", "file"))}<section class="c-panel"><div class="c-panel-head"><div class="tabs"><button class="tab" aria-pressed="true">All</button><button class="tab" aria-pressed="false">Failed</button><button class="tab" aria-pressed="false">Lifecycle</button></div><label class="search" style="max-width:300px">${icon("search")}<span class="visually-hidden">Search activity</span><input type="search" placeholder="Search activity"></label></div><div class="c-master-detail"><div class="c-master">${standardHistoryTable()}</div><aside class="c-detail"><p class="eyebrow">Job report</p><h2>northwind backup</h2><p class="muted">Today 21:42 · 8m 14s</p>${status("Succeeded", "good")}<h3>Targets</h3><dl><dt>Local vault</dt><dd>Success · 405 MB new</dd><dt>Wasabi EU</dt><dd>Success · 407 MB new</dd></dl><h3>restic output</h3><pre class="c-log">snapshot 40d8f2a1 saved\nFiles: 18,421 new, 104,283 unchanged\nAdded to repository: 812.4 MiB\nprocessed 8.4 GiB in 8:14</pre></aside></div></section>`;
}

function settingsC() {
  return `${consolePagebar(action("Save", "primary", "check"))}<section class="c-panel"><div class="c-panel-head"><h2>Node configuration</h2><span class="muted mono">/var/lib/cprest/state.db</span></div><div class="c-master-detail"><div class="c-master"><div class="card-pad stack-lg"><div class="grid-2"><div class="field"><label for="c-host">Hostname tag</label><input id="c-host" value="mx.7171.online"></div><div class="field"><label for="c-workers">Concurrent accounts</label><input id="c-workers" type="number" value="1"></div><div class="field"><label for="c-restic">restic path</label><input id="c-restic" value="/usr/local/bin/restic"></div><div class="field"><label for="c-days">Restore output days</label><input id="c-days" type="number" value="7"></div></div><fieldset style="border:0;padding:0;margin:0"><legend class="eyebrow">Lifecycle hooks</legend><label class="check-row"><input type="checkbox"><span><span class="check-title">Protect account removal</span><span class="check-copy">Block termination without current complete copies.</span></span></label><label class="check-row"><input type="checkbox"><span><span class="check-title">Backup on suspension</span><span class="check-copy">Queue a preservation plan after suspendacct.</span></span></label></fieldset></div></div><aside class="c-detail"><p class="eyebrow">Runtime</p><h2>Healthy</h2>${status("Service active", "good")}<h3>Paths</h3><dl><dt>State</dt><dd class="mono">/var/lib/cprest</dd><dt>Staging</dt><dd class="mono">/var/lib/cprest/staging</dd><dt>Cache</dt><dd class="mono">/var/cache/cprest/restic</dd><dt>Free</dt><dd>6.2 GB</dd></dl><h3>Notifications</h3><p>Operations email is working. Last test sent yesterday.</p>${action("Send test")}</aside></div></section>`;
}

function pageC() {
  return ({ overview: overviewC, accounts: accountsC, destinations: destinationsC, schedules: schedulesC, restore: restoreC, history: historyC, settings: settingsC }[page] || overviewC)();
}

function renderC() {
  const nav = pages.map(([key, label, iconName]) => `${pageButton(key, label, iconName)}${key === "accounts" ? "" : ""}`).join("");
  return `<div class="variant-c"><div class="c-shell"><header class="c-commandbar"><div class="brand"><span class="brand-mark">cP</span><span>Recovery console</span></div><label class="c-command">${icon("search")}<span class="visually-hidden">Search or run a command</span><input type="search" placeholder="Search account, snapshot, or command"><span class="key">/</span></label><div class="c-statusline">${status("Healthy", "good")}<span>root · mx.7171.online</span></div></header><div class="c-body"><nav class="c-nav" aria-label="cP:Restic pages"><div class="c-nav-label">Workspace</div>${nav}</nav><main id="prototype-main" class="c-workbench" tabindex="-1">${pageC()}</main></div></div></div>`;
}

const url = new URL(window.location.href);
let variant = variants[url.searchParams.get("variant")] ? url.searchParams.get("variant") : "A";
let page = pages.some(([key]) => key === url.searchParams.get("page")) ? url.searchParams.get("page") : "overview";
let restoreMode = ["single", "multiple"].includes(url.searchParams.get("restore")) ? url.searchParams.get("restore") : "";
let bulkScope = url.searchParams.get("scope") === "all" ? "all" : "selected";
let singleAccount = snapshots[url.searchParams.get("account")]?.length ? url.searchParams.get("account") : "northwind";
let singleSnapshot = snapshots[singleAccount][0].id;
let selectedRestoreParts = new Set(restoreParts.map((part) => part.key));
let selectedRestoreAccounts = new Set(["northwind", "meridian"]);
let bulkSnapshots = new Map(accounts.filter((account) => snapshots[account.name].length).map((account) => [account.name, snapshots[account.name][0].id]));
let toastTimer;

function updateURL() {
  const next = new URL(window.location.href);
  next.searchParams.set("variant", variant);
  next.searchParams.set("page", page);
  if (page === "restore" && restoreMode) {
    next.searchParams.set("restore", restoreMode);
    if (restoreMode === "multiple") {
      next.searchParams.set("scope", bulkScope);
      next.searchParams.delete("account");
    } else {
      next.searchParams.set("account", singleAccount);
      next.searchParams.delete("scope");
    }
  } else {
    next.searchParams.delete("restore");
    next.searchParams.delete("scope");
    next.searchParams.delete("account");
  }
  history.replaceState(null, "", next);
}

function render(focusMain = false) {
  document.getElementById("app").innerHTML = variant === "A" ? renderA() : variant === "B" ? renderB() : renderC();
  document.getElementById("variant-label").textContent = `${variant} — ${variants[variant]}`;
  document.title = `${pageMeta[page][0]} · ${variant} — ${variants[variant]} · prototype`;
  updateURL();
  if (focusMain) {
    document.getElementById("prototype-main").focus({ preventScroll: true });
    const behavior = window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth";
    window.scrollTo({ top: 0, behavior });
  }
}

function cycleVariant(direction) {
  const keys = Object.keys(variants);
  const current = keys.indexOf(variant);
  variant = keys[(current + direction + keys.length) % keys.length];
  render(true);
}

function showToast(message) {
  const toast = document.getElementById("prototype-toast");
  toast.textContent = `Prototype only — “${message}” did not run.`;
  toast.classList.add("show");
  window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => toast.classList.remove("show"), 3500);
}

document.addEventListener("click", (event) => {
  const cycle = event.target.closest("[data-cycle]");
  if (cycle) {
    cycleVariant(Number(cycle.dataset.cycle));
    return;
  }
  const restoreModeControl = event.target.closest("[data-restore-mode]");
  if (restoreModeControl) {
    restoreMode = ["single", "multiple"].includes(restoreModeControl.dataset.restoreMode) ? restoreModeControl.dataset.restoreMode : "";
    render(true);
    return;
  }
  const bulkScopeControl = event.target.closest("[data-bulk-scope]");
  if (bulkScopeControl) {
    bulkScope = bulkScopeControl.dataset.bulkScope === "all" ? "all" : "selected";
    render();
    document.querySelector(`[data-bulk-scope="${bulkScope}"]`)?.focus();
    return;
  }
  const partsAction = event.target.closest("[data-parts-action]");
  if (partsAction) {
    selectedRestoreParts = partsAction.dataset.partsAction === "all" ? new Set(restoreParts.map((part) => part.key)) : new Set();
    render();
    document.querySelector(`[data-parts-action="${partsAction.dataset.partsAction}"]`)?.focus();
    return;
  }
  const pageControl = event.target.closest("[data-page]");
  if (pageControl) {
    if (pageControl.dataset.page === "restore" && page !== "restore") restoreMode = "";
    page = pageControl.dataset.page;
    render(true);
    return;
  }
  const actionControl = event.target.closest("[data-action]");
  if (actionControl) showToast(actionControl.dataset.action);
});

document.addEventListener("change", (event) => {
  if (event.target.matches("[data-single-account]")) {
    singleAccount = event.target.value;
    singleSnapshot = snapshots[singleAccount][0]?.id || "";
    render();
    document.querySelector("[data-single-account]")?.focus();
    return;
  }
  if (event.target.matches("[data-single-snapshot]")) {
    singleSnapshot = event.target.value;
    render();
    document.querySelector("[data-single-snapshot]")?.focus();
    return;
  }
  if (event.target.matches("[data-restore-part]")) {
    const key = event.target.dataset.restorePart;
    if (event.target.checked) selectedRestoreParts.add(key);
    else selectedRestoreParts.delete(key);
    render();
    document.querySelector(`[data-restore-part="${key}"]`)?.focus();
    return;
  }
  if (event.target.matches("[data-bulk-account]")) {
    const accountName = event.target.dataset.bulkAccount;
    if (event.target.checked) selectedRestoreAccounts.add(accountName);
    else selectedRestoreAccounts.delete(accountName);
    render();
    document.querySelector(`[data-bulk-account="${accountName}"]`)?.focus();
    return;
  }
  if (event.target.matches("[data-bulk-snapshot]")) {
    bulkSnapshots.set(event.target.dataset.bulkSnapshot, event.target.value);
  }
});

document.addEventListener("submit", (event) => {
  event.preventDefault();
  showToast("Submit form");
});

document.addEventListener("keydown", (event) => {
  const target = event.target;
  if (target.matches("input, textarea, select, [contenteditable]")) return;
  if (event.key === "ArrowLeft") cycleVariant(-1);
  if (event.key === "ArrowRight") cycleVariant(1);
});

window.addEventListener("popstate", () => {
  const current = new URL(window.location.href);
  variant = variants[current.searchParams.get("variant")] ? current.searchParams.get("variant") : "A";
  page = pages.some(([key]) => key === current.searchParams.get("page")) ? current.searchParams.get("page") : "overview";
  restoreMode = ["single", "multiple"].includes(current.searchParams.get("restore")) ? current.searchParams.get("restore") : "";
  bulkScope = current.searchParams.get("scope") === "all" ? "all" : "selected";
  if (snapshots[current.searchParams.get("account")]?.length) {
    singleAccount = current.searchParams.get("account");
    singleSnapshot = snapshots[singleAccount][0].id;
  }
  render();
});

render();
