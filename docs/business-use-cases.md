# What Customers Can Do With PufferFS

PufferFS turns business files into searchable company knowledge. Customers can
sync folders that contain documents, PDFs, presentations, notes, images,
exports, and project files, then search them by meaning instead of relying on
exact filenames or exact wording.

This document focuses on customer-facing business value. It excludes account
setup, API details, platform administration, and low-level implementation
details.

## The Core Job

Most organizations already have valuable information scattered across local
folders, shared drives, project directories, exported reports, PDFs, decks,
notes, and client workspaces. The problem is not that the information is
missing. The problem is that people cannot reliably find it when they need it.

PufferFS helps customers:

- Find the right file without remembering its name.
- Find the right passage without opening dozens of documents.
- Search for an idea even when the file uses different words.
- Keep search results current as folders change.
- Give AI assistants and agents access to relevant business context.
- Reduce repeated questions inside teams.
- Preserve knowledge that would otherwise stay trapped in local files.

## Search Business Files by Meaning

Customers can search synced folders using natural-language intent, keywords, or
a hybrid of both.

What this enables:

- Search for "the contract clause about renewal notice" instead of guessing the
  exact file name.
- Find "the deck where we explained enterprise pricing" across presentations
  and PDFs.
- Locate "customer notes about SSO requirements" even if the notes use related
  language like authentication, identity provider, or Okta.
- Search broad folders without manually browsing nested directories.
- Get results that point back to the file, page, and relevant content.

Useful for:

- Sales teams searching proposals, pricing decks, call notes, and account
  research.
- Customer success teams searching onboarding notes, support histories, renewal
  context, and implementation details.
- Operations teams searching process docs, vendor files, invoices, forms, and
  internal runbooks.
- Legal and finance teams searching PDFs, agreements, reports, and exported
  records.
- Founders and executives searching across strategy docs, investor materials,
  research notes, and customer feedback.

## Find Answers Inside PDFs, Slides, Docs, and Images

Customers can index more than plain text. PufferFS can extract searchable
content from common business file types.

Supported content includes:

- PDFs.
- Word documents.
- PowerPoint presentations.
- Markdown and plain text notes.
- Images and screenshots when vision extraction is available.
- Code and configuration files when those are part of the workspace.

What this enables:

- Find information buried in long PDFs.
- Search presentation decks without opening each one.
- Recover details from screenshots, scanned pages, or image-heavy documents
  when extraction is available.
- Search mixed project folders where files are not neatly organized by type.
- Use one search workflow across documents, notes, and technical files.

Useful for:

- Client-facing teams managing deliverables and meeting materials.
- Finance teams searching statements, exports, and reports.
- Legal teams searching agreements and supporting documents.
- Operations teams searching policies, forms, and vendor paperwork.
- Research teams searching articles, PDFs, notes, and collected material.

## Make AI Assistants More Useful With Company Context

Customers can make local folders retrievable by AI assistants and agents. This
helps AI systems answer from real company material instead of relying on
generic knowledge or long pasted prompts.

What this enables:

- Let an assistant answer questions from synced company files.
- Retrieve only the relevant snippets instead of sending entire folders.
- Give agents fresher context as files change.
- Ground responses in the actual documents, notes, and records the team uses.
- Support workflows where a human or agent needs to find the right source
  material before drafting, replying, or making a decision.

Useful for:

- Sales assistants preparing for account calls.
- Support assistants finding prior customer context.
- Operations assistants answering policy or process questions.
- Research assistants summarizing relevant source material.
- Internal copilots that need access to local or team-specific knowledge.

## Keep Knowledge Current as Work Changes

Customers can update the searchable index incrementally instead of rebuilding
everything from scratch. PufferFS detects new, changed, moved, renamed, and
removed files, then updates only what changed.

What this enables:

- Keep search current as teams edit files throughout the day.
- Avoid stale answers from old folder snapshots.
- Avoid expensive full re-indexing of large workspaces.
- Run continuous sync for folders that change frequently.
- Keep long-running AI workflows grounded in current files.

Useful for:

- Active client workspaces.
- Shared operations folders.
- Sales enablement libraries.
- Support knowledge bases.
- Research folders that accumulate new material over time.

## Reduce Time Spent Asking Around

PufferFS helps people find existing knowledge before interrupting coworkers.

What this enables:

- Answer "where did we document this?" without messaging the team.
- Recover decisions from old notes or decks.
- Find the latest version of a process, policy, or explanation.
- Locate source material behind a recurring question.
- Help new teammates self-serve context faster.

Useful for:

- Onboarding new employees.
- Reducing repeated Slack or email questions.
- Preserving context when people change roles.
- Keeping institutional knowledge usable after projects end.
- Helping managers and operators find background quickly.

## Support Customer and Client Work

Customers can turn each client, account, or project folder into a searchable
workspace.

What this enables:

- Find prior commitments, requirements, or decisions for a customer.
- Search across proposals, meeting notes, contracts, and deliverables.
- Prepare faster for renewals, QBRs, audits, or handoffs.
- Locate historical context when a customer asks about something from months
  ago.
- Separate client workspaces while still making each one searchable.

Useful for:

- Agencies.
- Consultants.
- Customer success teams.
- Professional services teams.
- Account managers.
- Implementation teams.

## Improve Compliance, Audit, and Review Workflows

Customers can search across supporting materials when they need to review,
verify, or prove something.

What this enables:

- Find the document that supports a policy or decision.
- Locate agreements, reports, invoices, and audit evidence faster.
- Search for references to vendors, controls, terms, or obligations.
- Review folders before sharing, archiving, or deleting material.
- Reduce risk from missing relevant documents during review.

Useful for:

- Compliance reviews.
- Vendor reviews.
- Contract reviews.
- Finance audits.
- Internal policy updates.

## Keep Sensitive Boundaries Intact

Customers can organize search around shared team folders and user-owned
folders, with controls that limit what people can see.

What this enables:

- Make shared team knowledge searchable.
- Keep private workspaces separate.
- Hide sensitive path prefixes from search results.
- Let organization admins manage shared visibility.
- Avoid exposing unrelated or private files to broad searches.

Useful for:

- Teams that separate customer, department, or project folders.
- Organizations with mixed public/internal/private material.
- Agencies managing multiple clients.
- Companies that want AI assistants to retrieve context without overexposing
  unrelated files.

## Reduce Search Noise

Customers can keep irrelevant folders and files out of sync and search.

What this enables:

- Exclude cache folders, build outputs, dependencies, temporary files, and other
  noisy material.
- Respect existing project ignore rules.
- Add PufferFS-specific ignore rules.
- Exclude likely secret files such as `.env` and private keys by default.
- Filter results by file pattern when a folder contains many file types.

Useful for:

- Large shared folders with a mix of useful and irrelevant files.
- Teams that keep exports, generated files, or working files near final
  documents.
- Workspaces that include sensitive environment or credential files.

## Work From CLI, Background Sync, or Web Console

Customers can use PufferFS in a way that fits their workflow.

What this enables:

- Run sync and search from the command line.
- Keep selected folders current with `sync --follow`.
- Refresh one changed document quickly with `sync --root <folder> --only <file>`
  without scanning the full folder.
- Install background sync services on macOS or Linux.
- Use a web console for lightweight visibility into available roots, members,
  and billing.
- Integrate search into scripts or AI-agent workflows.

Useful for:

- Power users who prefer command-line tools.
- Teams that want certain folders kept current automatically.
- AI workflows that need a repeatable search command.
- Organizations that want simple visibility without making the web app the main
  work surface.

## Business Outcomes

PufferFS helps customers:

- Find answers faster across messy business folders.
- Make existing documents useful again.
- Reduce duplicated work and repeated questions.
- Make AI assistants more accurate with real company context.
- Preserve knowledge from old projects, customers, and employees.
- Prepare faster for sales calls, support escalations, audits, renewals, and
  handoffs.
- Search across mixed files instead of only one app, one repository, or one
  document system.

## What PufferFS Is Not

PufferFS does not delete original customer files when a synced root is removed.
It removes PufferFS copies, search artifacts, and index metadata.

PufferFS is not only for engineering teams. It can search code when code is
part of the workspace, but the broader value is making filesystem-backed
business knowledge searchable for teams and AI assistants.

PufferFS is not a replacement for a document management system, CRM, source
control platform, or shared drive. It is a search and retrieval layer for the
files and folders customers already use.
