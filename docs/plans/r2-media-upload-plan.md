# Kế hoạch upload ảnh và video trực tiếp lên Cloudflare R2

**Trạng thái:** Draft — chỉ kế hoạch, chưa triển khai mã nguồn  
**Phạm vi:** Guest upload ảnh/video cho event; private R2 delivery và gallery cơ bản  
**Kiến trúc:** Next.js 16 + React Query / Gin modular monolith + PostgreSQL + Redis + Cloudflare R2

> Chiến lược tách logical media khỏi storage provider, replication/backup sang Google Drive và migration provider được mô tả chi tiết tại [media-storage-portability-strategy.md](./media-storage-portability-strategy.md). Không thiết kế public link dựa trên R2 object key hay Google Drive file ID.

## 1. Kết quả cần đạt

Guest có thể mở link event, chọn hoặc chụp nhiều ảnh/video và upload trực tiếp từ trình duyệt lên private Cloudflare R2. API chỉ cấp quyền upload, lưu metadata, xác minh object sau upload và phục vụ gallery có kiểm soát; không proxy binary qua API server.

```text
Guest browser
  │
  ├─ GET  public event
  ├─ POST create/restore guest session
  ├─ POST request upload target (metadata only)
  ├─ PUT  file directly to private R2 bucket
  └─ POST complete upload
                 │
                 ├─ HeadObject verification
                 ├─ persist Media state and quota
                 └─ gallery refetch / future realtime event
```

## 2. Hiện trạng đã xác nhận trong codebase

### Backend (`candid-crowd-be`)

- `internal/media/service.go` đã có `CreateUpload` và `Complete`: kiểm tra event active, MIME/size, Redis rate-limit, quota reservation, presigned PUT, `HeadObject` verification và cập nhật quota.
- `internal/media/media.go` có entity `Media`; migration ban đầu đã có enum `pending`, `uploading`, `uploaded`, `ready`, `failed`, `deleted`.
- `internal/platform/r2/storage.go` hiện là adapter S3-compatible đúng vị trí; port `Storage` nằm ở package `media`.
- `internal/httpapi/public.go` và router đã khai báo:

  ```text
  GET  /api/v1/public/events/:slug
  POST /api/v1/public/events/:slug/sessions
  POST /api/v1/public/events/:slug/uploads
  POST /api/v1/public/events/:slug/uploads/:uploadId/complete
  ```

- `cmd/api/main.go` đã wiring R2, Redis limiter, guest service và media service.
- Chưa có Media repository riêng; `media.Service` đang gọi GORM trực tiếp. Chưa có stale-upload cleanup, public media gallery API, thumbnail/processing workflow hay test tích hợp upload thực.

### Frontend (`candid-cowd-fe`)

- `GuestEventView` đã có UI chọn nhiều file, camera, preview, per-item state, progress UI, retry UI và `CandidCameraModal`.
- `handleUploadSubmit` hiện chỉ giả lập upload bằng `setTimeout`, dùng `URL.createObjectURL()` và `localStorage`; chưa gọi API/R2.
- Guest page hiện dùng `useEvent` (host endpoint) và fallback event giả. Luồng production phải dùng `usePublicEvent` và không được âm thầm fallback vào mock data.
- FE đang giới hạn mọi file ở 50 MB và `accept="image/*,video/*"`; không khớp whitelist và limit BE hiện tại (JPEG/PNG/WebP/MP4/MOV; ảnh 25 MB, video 500 MB theo config mặc định).

## 3. Quyết định kiến trúc

### 3.1 Bounded context và dependency direction

Giữ `internal/media` là bounded context cho quyền upload, lifecycle, metadata và quota reservation.

```text
httpapi handler (Gin adapter)
       ↓
media use cases / service
       ↓                 ↓
MediaRepository port     Storage port
       ↓                 ↓
PostgreSQL/GORM adapter  Cloudflare R2 adapter
```

- Domain/use case không import Gin, AWS SDK, R2 hoặc GORM concrete types.
- `internal/platform/r2` là adapter duy nhất biết S3-compatible API của R2.
- R2 credentials chỉ có ở BE. FE chỉ nhận presigned URL ngắn hạn và required headers.
- `Event` giữ event policy/quota; `Media` là aggregate cho mỗi file. Không tạo wedding-specific model.

### 3.2 Media lifecycle

```text
pending
  → uploading       (client đã có upload target / bắt đầu PUT)
  → uploaded        (PUT thành công, complete đang xác minh)
  → processing      (async metadata/thumbnail/video processing; phase sau)
  → ready           (được phép xuất hiện trong gallery)

pending/uploading/uploaded/processing → failed
ready → deleted
```

P0 có thể chuyển ảnh trực tiếp từ `uploaded` sang `ready` sau `HeadObject`; video nên sớm chuyển qua `processing` khi thumbnail/poster đã được đưa vào scope. Gallery chỉ trả `ready`.

### 3.3 Object naming và persisted metadata

BE tạo key, không dùng filename của client làm key:

```text
events/{eventID}/media/{mediaID}/original.{extension}
events/{eventID}/media/{mediaID}/thumbnail.webp       # future
events/{eventID}/media/{mediaID}/poster.webp          # future
```

Persist stable metadata, không persist presigned URL:

```text
id, event_id, guest_session_id,
storage_provider, bucket, object_key,
original_filename, mime_type,
expected_size, actual_size, checksum,
status, width, height, duration,
created_at, uploaded_at, ready_at
```

## 4. API contract cần chốt

### Public event/session

```text
GET /api/v1/public/events/:slug
```

Trả event guest-safe, bao gồm `gallery_enabled` và một `upload_policy` để FE không hard-code limits.

```json
{
  "id": "uuid",
  "name": "Lan & Minh",
  "slug": "lan-minh",
  "event_date": "2026-10-01",
  "gallery_enabled": true,
  "upload_policy": {
    "accepted_mime_types": ["image/jpeg", "image/png", "image/webp", "video/mp4", "video/quicktime"],
    "max_image_bytes": 26214400,
    "max_video_bytes": 104857600,
    "max_files_per_batch": 20
  }
}
```

`POST /sessions` trả guest session token có expiry. FE lưu token theo slug; không đưa token vào URL, analytics hoặc log.

### Xin target upload

```text
POST /api/v1/public/events/:slug/uploads
```

```json
{
  "filename": "IMG_1024.HEIC.jpg",
  "mime_type": "image/jpeg",
  "size": 4281900,
  "guest_session_token": "..."
}
```

```json
{
  "media_id": "uuid",
  "upload_url": "https://...signed...",
  "expires_at": "2026-09-22T12:00:00Z",
  "required_headers": { "Content-Type": "image/jpeg" }
}
```

`Content-Type` phải được FE gửi chính xác vì nó là phần metadata đã ký và sẽ được kiểm tra bằng `HeadObject`.

### Upload complete

```text
POST /api/v1/public/events/:slug/uploads/:mediaId/complete
```

Request chỉ mang guest session token. Endpoint phải idempotent: nếu object đã được xác minh và Media `ready`, lần gọi retry vẫn trả success.

### Gallery (P0)

```text
GET /api/v1/public/events/:slug/media?cursor=...&limit=...
```

Chỉ trả media `ready`, cursor pagination, thumbnail URL có hạn và không lộ raw object key/bucket. Original private file được cấp signed GET ngắn hạn qua API khi cần.

## 5. Kế hoạch backend

### Phase BE-1 — Hoàn thiện persistence và service boundaries

1. Thêm `MediaRepository` port và GORM adapter; chuyển thao tác GORM ra khỏi `media.Service`.
2. Đồng bộ Go `Status` constants với toàn bộ enum migration.
3. Tách use case/command rõ ràng: `CreateUpload`, `CompleteUpload`, `FailUpload`, `ExpireStaleUploads`; phase sau có `CreateMultipartUpload`, `SignUploadPart`, `CompleteMultipartUpload`.
4. Thêm migration metadata cần cho provider/bucket/checksum/variants/processing timestamps; giữ migration append-only.
5. Quota reservation phải atomic và complete chỉ cộng actual size đúng một lần.

### Phase BE-2 — R2, authorization và hardening

1. `PresignPut` trả required headers cùng URL; `HeadObject` xác minh exact key, size và content type.
2. Validate `R2_ACCESS_KEY_ID` và `R2_SECRET_ACCESS_KEY` ở production, không chỉ endpoint/bucket.
3. Tất cả key do server tạo; filename được sanitize chỉ để display.
4. Rate-limit theo event + guest session + IP; thêm batch/concurrency policy.
5. Giới hạn thời hạn URL ngắn; timeout context cho I/O R2; log structured gồm request ID, event ID, media ID nhưng không log guest token/presigned URL.
6. Scheduled cleanup: các media pending/uploading/uploaded quá hạn phải bị mark failed và xóa R2 object nếu tồn tại; reservation quota được giải phóng.

### Phase BE-3 — Gallery và processing

1. Query gallery public qua Media repository, chỉ media `ready`, sort/cursor pagination.
2. Thêm controlled signed GET hoặc media delivery endpoint; R2 bucket luôn private.
3. P0: ảnh `HeadObject` thành công có thể `ready` ngay.
4. P1: job async đọc metadata an toàn, tạo thumbnail/poster, video state `processing → ready`; job failure không làm hỏng original.
5. Chỉ sau khi có processing/scan mới xem xét magic-byte sniffing, dimensions, checksum, antivirus/malware policy. FE MIME không phải security control.

## 6. Kế hoạch frontend

### Phase FE-1 — Data contract và guest session

1. Chuyển guest route sang `usePublicEvent`; bỏ fallback mock ở production. Mock chỉ nên là explicit test/preview fixture.
2. Tạo `useGuestSession(slug)`; tạo token khi upload lần đầu và cache theo slug với expiry.
3. Cập nhật types theo public event/upload policy và thêm React Query keys cho session, media gallery, media mutation.
4. Dùng policy từ API cho `accept`, validation sớm, copy lỗi và số file tối đa. BE vẫn validate lại.

### Phase FE-2 — Upload queue thực

Tạo feature riêng, ví dụ `src/features/upload/`, thay vì đặt HTTP/R2 orchestration trong `GuestEventView`:

```text
upload-api.ts          create session, request target, complete
direct-r2-upload.ts    direct XHR PUT, headers, progress, abort
use-upload-queue.ts    queue, retry, concurrency, state machine
types.ts                upload item and public API contract
```

State theo từng file:

```text
queued → authorizing → uploading → completing → ready
                                  ↘ failed / canceled
```

- Dùng `XMLHttpRequest` cho PUT trực tiếp để nhận `upload.onprogress`; không dùng axios API instance có auth interceptor cho presigned URL.
- P0 upload tuần tự hoặc tối đa 2 concurrent requests để tránh nghẽn Wi-Fi sự kiện.
- Retry PUT: xin target mới khi URL hết hạn hoặc retryable network error.
- Retry complete: chỉ gọi lại complete idempotently, không re-upload nếu object đã có.
- Hỗ trợ abort cho file đang upload; không hứa hẹn browser background upload, đặc biệt iOS Safari.
- `URL.createObjectURL` chỉ dùng preview và phải revoke khi remove/unmount. Gallery sau thành công dùng media record/API URL, không dùng blob URL/localStorage.

### Phase FE-3 — UX, resiliency, gallery

1. Giữ UI staging hiện có, thay progress mô phỏng bằng byte progress thật.
2. Hiển thị success/failed/cancel ở từng file và retry từng file hoặc retry failed.
3. Chỉ bật `beforeunload` khi request active; không chặn sau khi upload thành công.
4. Sau complete, invalidate/refetch public media query, rồi thêm item thật vào gallery.
5. Gallery dùng thumbnail; không preload originals hoặc autoplay video.
6. Phase sau: IndexedDB chỉ lưu metadata queue và khả năng resume intent sau reload, không cam kết upload background/offline.

## 7. Cloudflare R2 provisioning

- Bucket private; không gắn public bucket domain cho original objects.
- R2 CORS là cấu hình riêng với API CORS. Allow origins chỉ FE dev/prod đã biết.
- CORS R2 cần `PUT`, `GET`, `HEAD`; allow `Content-Type` và checksum header nếu ký checksum; expose `ETag` nếu client cần nó.
- API CORS giữ origin list tương ứng cho calls tạo session/target/complete.
- Secrets R2 lưu deployment secret manager; không commit `.env` hay gửi xuống Next.js.
- Kiểm thử từ browser origin thật vì Postman không phát hiện preflight/CORS lỗi.

### Configuration rule

Mọi credential và giá trị vận hành phải đi qua environment/configuration, không hard-code trong Go/TypeScript, migration, test fixture production hay tài liệu deploy:

```text
R2_ENDPOINT
R2_REGION
R2_BUCKET
R2_ACCESS_KEY_ID
R2_SECRET_ACCESS_KEY
R2_PRESIGN_EXPIRY
UPLOAD_MAX_IMAGE_BYTES
UPLOAD_MAX_VIDEO_BYTES
EVENT_MAX_MEDIA_BYTES
UPLOAD_RATE_LIMIT
UPLOAD_RATE_WINDOW
CORS_ALLOWED_ORIGINS
```

- Frontend chỉ có các biến `NEXT_PUBLIC_*` an toàn để public, ví dụ `NEXT_PUBLIC_API_BASE_URL`; không bao giờ expose R2 credentials, bucket secret, presigned URL mặc định hay guest token.
- Validate required environment variables khi application khởi động; production phải fail fast nếu thiếu credential/endpoint/bucket cần thiết.
- Giá trị không bí mật nhưng thay đổi theo môi trường (limits, expiry, allowlist, concurrency) cũng đọc từ config để vận hành mà không sửa code.

## 8. Chiến lược file size và multipart

P0 nên đặt giới hạn video direct PUT thực tế thấp hơn 500 MB (ví dụ 100–200 MB) để giảm tỷ lệ lỗi ở Wi-Fi venue. Dùng single PUT cho ảnh và video nhỏ.

Khi product data chứng minh cần video lớn hoặc bad-network resume, triển khai S3-compatible multipart upload:

```text
create multipart upload → presign each part → upload parts with retry
→ complete multipart upload → API HeadObject verification → processing
```

Không làm multipart ở P0 nếu chưa cần, vì nó tăng API surface, state cleanup và recovery complexity đáng kể.

## 9. Security checklist

- [ ] Private R2 bucket; no public original URL.
- [ ] Server-generated object key; client filename không ảnh hưởng path.
- [ ] Short-lived presigned URLs; exact event/session authorization.
- [ ] MIME whitelist + size limit ở FE và authoritative validation ở BE.
- [ ] HeadObject verifies expected size/Content-Type before `ready`.
- [ ] Rate limit session/event/IP, batch limit và concurrent limit.
- [ ] Guest token không nằm trong URL/log/analytics.
- [ ] Idempotent complete; quota only counted once.
- [ ] Stale records/object cleanup.
- [ ] Signed GET/controlled media delivery; gallery never exposes R2 credentials/key.
- [ ] Async decoding/scanning có memory/pixel limits; không decode arbitrary file trong HTTP request path.

## 10. Testing và acceptance criteria

### Backend

- Unit tests với in-memory repository/storage port: allowed MIME, limits, event state, quota, duplicate complete, stale cleanup.
- Gin handler tests: malformed input, invalid/expired session, wrong event, rate limit, object mismatch, idempotent complete.
- R2 adapter integration test với test bucket hoặc S3-compatible local service.
- Migration test: fresh database và upgrade path.

### Frontend

- Unit tests: queue transitions, retry selection, abort, expired target refresh, public policy validation.
- Playwright: select multiple files, direct PUT mock, true per-file progress, partial failure, retry, camera file path, navigation warning only while active.
- Browser test against R2 dev bucket: verify OPTIONS preflight, PUT with signed `Content-Type`, complete and gallery fetch.

### Done criteria P0

- Guest không cần account vẫn upload được JPEG/PNG/WebP/MP4/MOV theo policy.
- Binary không đi qua Gin server.
- Media chỉ xuất hiện ở gallery sau server verification.
- FE hiển thị progress thật, retry thật và lỗi từng file.
- Original media private; gallery delivery controlled.
- Quota, rate limit, guest isolation và stale cleanup có test.
- Không có mock/localStorage/blob URL nào được dùng làm persistence cho upload production.

## 11. Thứ tự triển khai đề xuất

1. R2 private bucket + CORS dev/prod + thử signed PUT từ browser.
2. Chốt OpenAPI/TypeScript contract cho public event, session, upload target và complete.
3. BE persistence/service boundary, idempotency, test handlers và stale cleanup.
4. FE guest session + `usePublicEvent`, thay mock upload bằng direct upload queue.
5. Public gallery media endpoint + controlled signed delivery.
6. Observability: upload success rate, stage latency, R2/complete errors, retry count.
7. Thumbnail/video processing, SSE/polling, IndexedDB metadata persistence.
8. Multipart upload khi metrics xác nhận single PUT không đủ cho video lớn/bad network.

## 12. Ngoài phạm vi P0

- Native mobile background uploads.
- Face recognition, AI curation, duplicate detection.
- Full offline uploads.
- Video transcoding pipeline phức tạp.
- Cross-event public social gallery.
- Multipart/resumable upload nếu single PUT limits vẫn đáp ứng product validation ban đầu.
