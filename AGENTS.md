# CandidCrowd — Coding Context

## Product in one sentence

**CandidCrowd** is a web platform that lets people at a real-world event share photos and videos into one private event gallery via a QR code or link, with **no guest account and no app install required**.

Core guest flow:

```text
Scan QR / open link
→ Take or choose photos/videos
→ Upload
→ Done
```

The long-term product supports many event types, but **go-to-market starts with weddings**.

## Product vision

CandidCrowd is not just a wedding photo album.

Long-term positioning:

> **Shared memories for every event.**

Initial positioning:

> **Get the photos your guests already take.**

Product philosophy:

> **Capture more memories without changing the event.**

The product should stay in the background and make contribution effortless.

## Market scope

Initial market: weddings.

Later event types:
- Birthdays
- Anniversaries
- Baby showers
- Graduations
- Family reunions
- Trips
- Company parties
- Team building
- Corporate events
- Conferences

### Architecture rule

The core domain must stay generic.

Use entities such as:

```text
Event
Host
GuestSession
Media
QRSource
EventMode
Prompt
```

Do **not** hard-code concepts such as `Bride`, `Groom`, or wedding-only domain models. Wedding-specific behavior should be templates/configuration.

## Main user problem

The problem is not storage. People already have Google Photos, Drive, WhatsApp, iMessage, etc.

The real problem is:

> Guests take many photos and videos during an event, but the host never receives most of them.

CandidCrowd reduces the friction required to contribute those memories.

## Core users

### Host
- Create an event quickly
- Get a QR code / guest link
- View incoming media
- Moderate media
- Download media
- See participation analytics
- Send post-event reminders

### Guest
Guests do not need an account.

They should be able to:
- Open guest page instantly
- Take a photo
- Select existing media
- Upload multiple files
- See upload progress
- Retry failed uploads
- Optionally view event gallery

### Professional users — later
- Photographers
- Event planners
- Venues
- Agencies
- Corporate event teams

# Current development priority

We are starting implementation now.

## Immediate milestone

Build a polished **public homepage + working product skeleton**.

Do not try to implement the full long-term roadmap immediately.

Current priorities:

1. Homepage
2. Authentication for host
3. Create generic event
4. Guest event page
5. QR/link generation
6. Direct media upload to Cloudflare R2
7. Event gallery
8. Basic moderation
9. Basic download
10. Basic realtime/live wall

Advanced AI, native mobile apps, face recognition and complex professional features are out of scope for now.

# Homepage

Recommended order:

```text
Header
↓
Hero + live product preview
↓
Interactive guest demo
↓
Pain / why this product exists
↓
How it works
↓
Why CandidCrowd is different
↓
Live event experience
↓
Before / During / After event lifecycle
↓
Privacy + ease of use
↓
Event use cases
↓
Pricing
↓
Final CTA
↓
Footer
```

For the first implementation, minimum homepage:

```text
Header
Hero
Interactive Demo
Pain
How It Works
Why CandidCrowd
Privacy / Ease
Pricing
Final CTA
```

## Homepage messaging

Suggested hero headline:

> **Get the photos your guests already take.**

Suggested description:

> One QR code gives everyone at your event a simple place to share their photos and videos. No app. No account.

Primary CTA:

> **Create your event free**

Supporting trust line:

```text
No app · Private · Original quality
```

## Visual direction

CandidCrowd should feel like:

> **Editorial photography × premium consumer product**

Prefer:
- Warm/off-white backgrounds
- Strong photography
- Spacious layouts
- Serif display type for emotional headlines
- Clean sans-serif for product UI
- Subtle motion
- Masonry photo layouts
- Real candid-event imagery

Avoid:
- Generic purple/blue SaaS gradients
- Too many feature cards
- Excessive icons
- Fake dashboards full of charts
- Heavy animation
- Overly corporate styling

The website should feel like a product about memories, not enterprise software.

# Three.js guidance

Three.js is optional and only for visual enhancement.

Recommended hero use:
- Slightly 3D phone mockup
- Floating photo cards
- Photos visually moving from a guest phone into a shared gallery
- Very subtle pointer parallax

The 3D scene should explain:

```text
Guest captures photo
→ photo is uploaded
→ photo appears in shared gallery
```

Do not build a full 3D website.

Rules:
- 3D must never block LCP
- Hero text and CTA render before 3D
- Lazy-load the Three.js scene
- Provide a static fallback
- Reduce/disable on low-end mobile devices
- Respect `prefers-reduced-motion`
- No heavy post-processing
- No large textures
- Pause animation when tab is hidden

Preferred React stack if needed:

```text
three
@react-three/fiber
@react-three/drei
```

Target balance:

```text
90% normal web UI / photography
10% Three.js
```

# MVP features

## Host

### Authentication
- Sign up
- Login
- Logout

### Event
- Create event
- Event name
- Event date
- Event type
- Expected guest count
- Public guest URL
- QR code

### Event management
- View media
- Delete/moderate media
- Basic event settings
- Download media

### Analytics
Initially:
- Number of scans
- Number of contributors
- Total media
- Photos per contributor

## Guest

Guests should not create accounts.

Guest flow:

```text
Open guest URL
→ enter event
→ choose/take media
→ upload
→ success
```

Features:
- Camera capture
- Multi-file upload
- Photo upload
- Video upload when enabled
- Upload progress
- Retry
- Mobile-first UI
- Optional gallery browsing
- Anonymous guest session/token

# Event modes

## Silent Mode
Mainly for wedding ceremonies.

Behavior:
- No prompts
- No live-wall encouragement
- No gamification

Possible message:

> Enjoy the moment. You can share your photos afterward.

## Soft Mode
Suitable for cocktail hour / early reception.

Behavior:
- Gentle CTA
- QR upload
- No aggressive prompts

## Social Mode
Suitable for reception / casual event periods.

Behavior:
- Live Wall
- Reactions
- Optional prompts

## Party Mode
Suitable for dance floor / afterparty.

Behavior:
- More playful prompts
- Optional photo missions

## After-event Mode
Important core workflow.

Guests often do not want to upload during the event.

Hosts should be able to send a reminder:

> Got photos from last night? Share them here.

Post-event contribution is a core feature, not an afterthought.

# Differentiation

Do not position CandidCrowd as only:

```text
QR + upload + gallery
```

Those are commodity features.

## Participation Engine

Measure and improve how many guests actually contribute.

Primary metric:

```text
participation_rate =
unique_contributing_guests / expected_guest_count
```

Secondary metrics:
- Media per event
- Media per contributor
- Guest → future host conversion

## QR source analytics

Support multiple QR codes for one event.

Examples:

```text
entrance
table
bar
dance_floor
screen
invitation
```

All QRs open the same event, but contain a source identifier.

Measure:

```text
QR source
→ scans
→ contributors
→ uploads
```

## Coverage Engine — later

Long-term system may detect missing event coverage.

Examples:
- Important moment has very few photos
- One area of the event has low contribution
- Some event segment has poor coverage

Do not publicly shame individuals or tables.

Coverage information is primarily for host, photographer, and planner.

# AI strategy

Do **not** add AI just for marketing.

AI is not required for MVP.

Potential later uses:
- Duplicate detection
- Blur detection
- Quality ranking
- Auto categorization
- Highlight selection
- Event timeline/story
- Coverage analysis

Do not implement face recognition in MVP.

If face-search is ever added later, it must use explicit opt-in because of biometric/privacy concerns.

# Technical architecture

## Frontend

Preferred:

```text
Next.js
TypeScript
React
```

Mobile-first responsive web app.

PWA features can be added progressively.

### CSS & Class Naming Conventions (BEM Standard)

All custom CSS classes must strictly adhere to the **BEM (Block Element Modifier)** convention:

- **Block (`block-name`)**: Independent standalone entity (kebab-case).
  - Examples: `site-header`, `hero`, `guest-demo`, `button`, `plan-card`, `event-form`.
- **Element (`block-name__element-name`)**: Part of a block that has no standalone meaning, tied to its block with two underscores `__`.
  - Examples: `site-header__inner`, `site-header__actions`, `hero__copy`, `hero__actions`, `plan-card__price`, `event-form__field`.
  - **No deep nesting**: Never use `block__elem1__elem2`. Use only `block__element`.
- **Modifier (`block-name--modifier` or `block-name__element--modifier`)**: State or variation flag, separated with two dashes `--`.
  - Examples: `button--small`, `button--outline`, `plan-card--featured`, `hero-scene--3d`, `hero-scene__tile--new`.
- **Global / Utility**: Shared layout classes should remain standard and semantic (e.g., `container`, `section-pad`, `skip-link`, `eyebrow`, `center-heading`).
- **Feature-first organization**: Component classes should reflect their feature/block context. Do not invent loose, un-scoped ad-hoc class names.

## Backend

Preferred:

```text
TypeScript / Node.js
```

Use PostgreSQL for core metadata.

Keep domain logic separated from infrastructure.

# Storage

Production storage is:

# Cloudflare R2

Use **presigned URLs**.

The application server must not proxy large media uploads.

Correct flow:

```text
Guest browser
    ↓
request upload permission
    ↓
API
    ↓
presigned PUT URL
    ↓
Guest browser ─────────→ Cloudflare R2
```

After upload:

```text
Browser
→ notify API / complete upload
→ API verifies and marks media ready
```

Benefits:
- No media binary through application server
- Lower application bandwidth
- Handles event upload bursts better
- Easier scaling
- R2 has zero Internet egress fees

# Storage abstraction

Do not make product/domain code depend directly on R2.

Use a storage abstraction similar to:

```ts
interface StorageProvider {
  createUploadTarget(input: CreateUploadInput): Promise<UploadTarget>;
  getDownloadUrl(key: string): Promise<string>;
  delete(key: string): Promise<void>;
}
```

Initial implementation:

```text
R2StorageProvider
```

This allows provider migration later without changing product logic.

# Media database rules

Do not store temporary presigned URLs.

Store stable storage metadata.

Example:

```text
Media
- id
- event_id
- uploader_session_id
- storage_provider
- bucket
- object_key
- mime_type
- size
- checksum
- status
- width
- height
- duration
- created_at
```

Example object key:

```text
events/{eventId}/media/{mediaId}/original.jpg
```

Possible media states:

```text
pending
uploading
uploaded
processing
ready
failed
deleted
```

# Privacy / media delivery

R2 bucket should be private.

Do not expose a fully public bucket.

Gallery requests should resolve media through controlled application logic.

Later options:
- Signed GET URLs
- CDN-backed media endpoint
- Image transformations
- Thumbnail variants

Host controls whether guests can view the event gallery.

# Upload reliability

Event venues often have poor Wi-Fi/mobile networks.

Guest upload UX must handle this well.

Implement progressively:
- Upload progress
- Retry
- Persist upload state
- File validation
- Client-side image compression where appropriate
- Concurrent upload limits
- Abort/retry controls
- IndexedDB for pending upload metadata when useful

Do not claim the web app is fully offline.

iOS Safari cannot provide the same reliable background upload behavior as a native app.

Correct product promise:

> **Resilient on bad venue Wi-Fi.**

# Realtime

Live Wall can use:

```text
SSE
or
WebSocket
```

Do not over-engineer realtime for the first version.

Event-scoped realtime coordination can later use Cloudflare Durable Objects if necessary.

Typical realtime events:

```text
media.created
media.ready
media.deleted
event.prompt.updated
```

# Background jobs

Use async jobs for expensive work.

Examples:
- Thumbnail generation
- Video processing
- ZIP export
- AI analysis
- Duplicate detection

These must not block upload completion or normal API requests.

# Suggested core data model

```text
User
Organization
Event
EventTemplate
GuestSession
QRSource
UploadSession
Media
MediaVariant
EventMode
EventPrompt
Reaction
ModerationAction
Plan
Subscription
EventAnalytics
```

Do not model the application around weddings.

Use event type/template configuration.

# Suggested API surface

Exact naming may change.

## Host

```text
POST   /api/auth/*
POST   /api/events
GET    /api/events/:eventId
PATCH  /api/events/:eventId
DELETE /api/events/:eventId

GET    /api/events/:eventId/media
GET    /api/events/:eventId/analytics
POST   /api/events/:eventId/qr-sources
```

## Guest

```text
GET    /api/public/events/:slug
POST   /api/public/events/:slug/session
POST   /api/public/events/:slug/uploads
POST   /api/public/events/:slug/uploads/:uploadId/complete
GET    /api/public/events/:slug/media
```

## Media

```text
DELETE /api/media/:mediaId
POST   /api/events/:eventId/export
```

Prefer IDs internally and human-friendly slugs externally.

# Security basics

Implement from the start:
- Private storage
- Short-lived presigned URLs
- File MIME validation
- File size limits
- Server-generated object keys
- Rate limiting
- Upload abuse protection
- Authorization on every host action
- Guest session isolation
- Safe media deletion
- Logging for moderation actions

Never trust the client-provided file name as the storage key.

# Performance priorities

Especially important on mobile.

Targets:
- Fast first paint
- Hero must not wait on Three.js
- Guest upload screen must load quickly
- Lazy-load galleries
- Use thumbnails, not originals, in grids
- Virtualize very large galleries when necessary
- Avoid loading full-size videos automatically

The upload page is more important than decorative animations.

# Product phases

## P0 — Prototype

Goal:

```text
QR/link
→ guest upload
→ R2
→ gallery
```

No AI.

No mobile app.

No complex plans.

## P1 — Public Wedding MVP

Add:
- Host auth
- Event management
- Payment
- Gallery
- Live Wall
- Download
- Moderation
- Basic event settings

Goal:

> Real weddings can use the product end-to-end.

## P2 — Participation

Add:
- Expected guest count
- QR sources
- Scan tracking
- Contributor tracking
- Participation dashboard
- Gentle prompts
- Post-event reminder

Goal:

> Prove CandidCrowd can increase guest contribution.

## P3 — Multi-event Consumer

Add templates for:
- Birthdays
- Graduations
- Baby showers
- Reunions
- Anniversaries

Do not rewrite the core event engine.

## P4 — Professional

For:
- Photographers
- Planners
- Venues

Add:
- Multi-event dashboard
- Branding
- Templates
- Team access
- Subscriptions

## P5 — Intelligence

Add:
- Curation
- Deduplication
- Highlights
- Event story
- Coverage Engine

## P6 — Corporate + Mobile

Add:
- Corporate workflows
- Large-event management
- Team roles
- Native mobile apps if product data proves they are needed

# Non-goals for first release

Do not implement yet:
- Native iOS app
- Native Android app
- Face recognition
- Complex AI pipeline
- Advanced video editor
- Social network features
- Public user profiles
- Marketplace
- Photographer CRM
- Seating planner
- RSVP system
- Full wedding-planning platform
- Complex white-label system
- Advanced enterprise permissions

Keep the product focused.

# UX principles

Guest flow must be extremely simple.

A non-technical older guest should be able to:

```text
scan
→ choose photo
→ upload
```

without instructions.

Rules:
- No guest signup
- No OTP
- No email requirement
- No unnecessary onboarding
- Large tap targets
- Mobile-first
- Clear upload state
- Friendly error recovery

# Main product metrics

Primary:

```text
Participation Rate
=
unique contributors / expected guests
```

Track:
- Events created
- Paid events
- QR scans
- Unique guest sessions
- Contributors
- Media uploaded
- Photos per contributor
- Upload success rate
- Guest → future host conversion
- Repeat host rate
- Professional events / month

# Current coding decision summary

Use these decisions unless explicitly changed later:

```text
Brand: CandidCrowd
Product: Event photo/video contribution platform
GTM: Wedding-first, generic event architecture
Frontend: Next.js + TypeScript
Storage: Cloudflare R2
Uploads: Browser → R2 via presigned PUT URL
Bucket: Private
Database: PostgreSQL
Guests: No account required
Mobile: Web-first
AI: Not MVP
Face recognition: Not MVP
3D: Optional lightweight Three.js hero enhancement only
Primary metric: Guest participation rate
```

# Coding priority

When choosing between beautiful architecture and a working guest upload, choose the working guest upload.

When choosing between more features and less guest friction, choose less guest friction.

When choosing between fancy homepage animation and fast mobile performance, choose fast mobile performance.

The first product hypothesis to validate is:

> **Will guests actually contribute their photos through CandidCrowd?**

Everything else comes after that.

# Package and Library Utilization Rule

- **Proactively use optimized existing libraries**: Do not reinvent the wheel or write custom boilerplate for features that already have mature, battle-tested, and performant libraries (e.g., shadcn/ui components, Lucide icons, Framer Motion, date-fns, zod, browser image compression, etc.).
- **Autonomous installation**: You are explicitly authorized and encouraged to automatically download and install needed npm packages (e.g. `npm install ...` or `npx ...`) to ensure fast, optimal, and reliable implementation instead of writing custom low-level replacements.

<!-- BEGIN:nextjs-agent-rules -->

# This is NOT the Next.js you know

This version has breaking changes — APIs, conventions, and file structure may all differ from your training data. Read the relevant guide in `node_modules/next/dist/docs/` (resolved from this file's directory; in monorepos the `next` package may not be visible from the repo root) before writing any code. Heed deprecation notices.

This block is written and re-added by `next dev` — verify at `node_modules/next/dist/server/lib/generate-agent-files.js`. Removing it from a diff only re-creates the uncommitted change; committing it with your work keeps the tree clean.

<!-- END:nextjs-agent-rules -->
