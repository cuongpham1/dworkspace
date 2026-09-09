<p align="center">
  <b>Sáu Vườn Ươm — nền tảng tri thức và quản trị chung cho mọi công cụ AI trong tổ chức.</b><br>
  Tài liệu, database và cộng tác realtime cho cả người và AI agent — cùng một workspace,
  cùng một bộ rule, cùng một cổng MCP duy nhất.
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> ·
  <a href="#what-an-agent-can-do">Agents</a> ·
  <a href="#những-gì-đội-đã-tự-xây-thêm">Đội đã xây thêm gì</a> ·
  <a href="#về-bản-fork-này">Về bản fork này</a>
</p>

---

## Những gì đội đã tự xây thêm

- **Human Publish Gate** — khi một AI agent (qua API token) tạo trang mới hoặc thay toàn bộ
  nội dung một trang, hệ thống không ghi thẳng: nó tạo một **đề xuất chờ duyệt** kèm bản so
  sánh (diff), chỉ người dùng qua trình duyệt mới publish hoặc reject được. Các thao tác ít
  rủi ro (nối thêm nội dung, comment, cập nhật property) vẫn đi thẳng, không bị chặn.
- **Skill Control Plane — 62 skill thật, đã duyệt** — một "skill" là một entity có vòng đời
  riêng (draft → pending review → approved → deprecated) trong bảng riêng, **không phải một
  trang tài liệu gắn thêm field trạng thái** — nên không ai tự "duyệt" skill bằng cách gõ tay
  một nhãn. Route thay đổi vòng đời skill chỉ chạy được qua phiên đăng nhập trình duyệt, một
  agent không tự duyệt được skill của chính nó. Workspace hiện có 62 skill đã duyệt, phủ toàn
  bộ vòng đời phát triển phần mềm — Discovery, Business Analysis, Product, Architecture,
  Engineering, QA, Operations.
- **Comment theo từng đoạn nội dung cụ thể (section-level)** — mỗi đoạn được comment có một
  thread trao đổi riêng, không gộp chung một dòng thời gian; giao diện tô sáng đúng đoạn văn
  bản tương ứng kiểu Google Docs, vị trí luôn khớp với văn bản thật kể cả khi trình soạn thảo
  build lại DOM ngầm.
- **Mở rộng MCP tooling cho Skill Control Plane** — bốn tool mới: `skill_catalog`,
  `skill_resolve`, `skill_get`, `skill_client_package`, theo đúng nguyên tắc "progressive
  disclosure" (metadata trước, nội dung đầy đủ khi thật sự cần).
- **Đa ngôn ngữ 3 thứ tiếng** (Việt / Anh / Đức) — mọi chuỗi hiển thị mới được kiểm tra tự
  động để không sót bản dịch trước khi phát hành.

Chi tiết kỹ thuật và các fix kế thừa từ nhánh gốc: xem [Về bản fork này](#về-bản-fork-này)
bên dưới.

---

An agent asks to organise the launch notes. A page appears, a database is
created, the rows fill in. A person opens the same board a second later and
carries on editing. Same pages, same permissions, same history.

That is the whole idea. Everything below is how it works.

## Why dworkspace exists

Agents increasingly need somewhere to put durable, structured work. Not a chat
log, not a vector store, but pages and tables a person will read tomorrow.

Today they get one of two bad options. A workspace built for humans, with an AI
feature bolted to the side, which means the agent talks *about* the content
through a chat window. Or agent infrastructure with a decent API and no
interface a human being would willingly use.

dworkspace is one workspace with two front doors. A block editor, databases and
realtime editing for people. An MCP endpoint for agents, on the same objects
and the same permission model. And you run the whole thing yourself.

## What an agent can do

Connect any MCP client to `/mcp` and it gets **33 tools** over the same
workspace you use:

- **Read and write pages**: create, update, move, duplicate, trash, restore
- **Work with databases**: create one, change its schema, add and query rows,
  configure views
- **Search** the whole workspace, with the same permission checks a person gets
- **Import** from a URL or a Notion export, in bulk
- **Comment**, and write to a page's append-only note trail
- **Announce what it is working on**, which shows live in the interface beside
  the page, so you can see an agent is mid-edit before you start typing
- **Discover and fetch an approved Skill** — `skill_catalog`, `skill_resolve`,
  `skill_get`, `skill_client_package` (added in this fork; see above)

**And a bounded set it cannot touch.** An agent may not create or delete
accounts, change two-factor settings, issue API tokens, take or restore a
backup, alter instance settings, or change who is in a workspace. That list is
not a promise in a README. The server sends it to every agent that connects.

Full reference: [MCP tools](https://salt.md/wiki/mcp-tools/).

## Agents get permissions, not a master key

A credential belongs to a person and carries that person's access, never more.
Beyond that:

- Every workspace decides for itself what agents may do there: anything they
  were granted, only signed-in connections, or nothing at all.
- Tokens narrow by scope and by workspace.
- Agent actions are attributable: the activity log distinguishes them from
  yours.
- Administration is deliberately out of reach of any token.

Giving an agent write access is only useful if you can still say who reached
what, and what changed. See [Permissions](https://salt.md/wiki/permissions/).

## And it is a real workspace

Not developer infrastructure with a login screen.

**Write.** A block editor with a slash menu, nested lists, checklists, quotes,
code, tables, images and callouts. Page links, backlinks, tags, covers and
icons. Comments in a side panel, threaded per section.

**Organise.** Turn any page into a collection with typed properties: text,
number, select, multi-select, date, person, checkbox, checklist, URL, relation,
rollup, formula and backrelation. Look at it as a table, board, list, gallery,
calendar, timeline or form. Filter, sort, group.

**Together.** Realtime editing with live cursors, comments, page history and an
activity log. Share a page publicly with an optional password and expiry.

Full-text search covers page text and the contents of uploaded PDFs, with
German stemming so *Verträge* finds *Vertrag*.

## Architecture

```
   Claude · ChatGPT · Cursor · any MCP client
                     │
                    MCP
                     │
              ┌─────────────┐
   people ──▶ │   dworkspace   │ ◀── REST API
    (browser) └─────────────┘     webhooks · ICS
                     │
            SQLite file + uploads
```

One Go process. `CGO_ENABLED=0`, so the binary is static and the SQLite driver
is pure Go. The frontend is embedded in it. Backing up is copying one file and
one directory.

No PostgreSQL, no Redis, no object store, no separate collaboration server.

## Self-hosting

| | |
| --- | --- |
| Install | one binary, `install.sh`, or the Docker image |
| Data | one SQLite file plus an uploads directory |
| Update | swap the binary or pull the image, restart |
| Backup | stop, copy two paths, start |
| Platforms | Linux, macOS and Windows, amd64 and arm64 |

A desktop application for macOS is available too. It is a window onto a server
you run, not a second copy of the product. See
[The desktop app](https://salt.md/wiki/desktop-app/).