# Chiến lược media độc lập storage provider và backup đa nền tảng

**Trạng thái:** Draft — chiến lược kiến trúc, chưa triển khai mã nguồn  
**Phạm vi:** Logical media URL, R2 hot storage, Google Drive cá nhân là archive tier theo daily migration và provider tương lai  
**Liên quan:** [Kế hoạch upload trực tiếp R2](./r2-media-upload-plan.md)

## 1. Quyết định cốt lõi

`Media` là tài sản logic của CandidCrowd, không phải một object của R2, Google Drive hay bất kỳ nhà cung cấp nào.

- FE, API public, gallery và host dashboard chỉ dùng `media_id`/`variant_id` do CandidCrowd phát hành.
- Không API nào trả raw R2 object key, bucket, Google Drive `fileId`, provider URL hoặc permanent provider link.
- Mỗi original/thumbnail/video poster có thể có nhiều physical copies (replicas) ở nhiều provider.
- Một copy được chọn làm **primary delivery replica**; các copy còn lại là backup, cold/archive hoặc replica đang chuẩn bị migration.
- Archive migration là eventual consistency, không nằm trên critical path của guest upload. Upload thành công vào R2 và server verification là đủ để media trở thành `ready`; daily job sẽ chuyển eligible media sang Google Drive, verify, rồi xóa R2 copy.

```text
Guest / Host / Gallery
        │                  chỉ biết logical CandidCrowd identity
        ▼
GET /media/:mediaId/variants/:variantName
        │
        ▼
Media delivery resolver
        │  chọn replica theo policy, availability, cost, region, lifecycle
        ├──────────────► R2 primary replica
        ├──────────────► Google Drive backup replica
        └──────────────► future: S3 / B2 / Azure Blob / archive
```

## 2. Public link không được phụ thuộc provider

Provider URL gắn product với storage vendor: không thể migrate mà không đổi link, signed URL hết hạn, permanent URL rủi ro privacy, và không thể lựa chọn bản sao tối ưu tại runtime.

Public API phải dùng logical application URL:

```text
GET /api/v1/media/{mediaId}/variants/thumbnail
GET /api/v1/media/{mediaId}/download
```

Endpoint xác thực quyền, resolve replica rồi redirect đến signed URL ngắn hạn hoặc stream qua media gateway. Client không lưu provider URL như dữ liệu lâu dài.

Google Drive là archive/backup provider theo chính sách của chủ hệ thống, **không** là CDN/gallery delivery mặc định. Sau khi R2 copy bị xóa, delivery resolver phải hoặc cấp controlled Drive read target, hoặc restore/cache tạm về R2 trước khi phục vụ gallery/download. Public link không đổi trong cả hai trường hợp.

## 3. Domain model

Tách identity, representation và physical location. Không thêm `r2_object_key` hay `google_drive_file_id` trực tiếp vào aggregate `Media`.

```text
MediaAsset (logical aggregate)
  ├─ event_id, uploader_session_id, visibility, moderation status
  ├─ original checksum/size/MIME, lifecycle status
  └─ MediaVariant (original | thumbnail | poster | transcoded-video)
       └─ StorageReplica (1..n physical copies)
            ├─ provider: r2 | google_drive | s3 | ...
            ├─ opaque provider location ID/key
            ├─ role: primary | backup | archive | migration-source | migration-target
            └─ state: pending | copying | verified | unavailable | deleting | deleted | failed
```

### Persistence tối thiểu

```text
media
  id, event_id, guest_session_id, original_filename,
  mime_type, expected_size, actual_size, checksum,
  status, created_at, uploaded_at, ready_at, deleted_at

media_variants
  id, media_id, name, mime_type, size, checksum,
  width, height, duration, status, created_at

storage_replicas
  id, variant_id,
  provider, location_reference, provider_metadata,
  role, storage_class, state,
  size, checksum, encryption_key_reference,
  verified_at, last_verified_at, created_at, deleted_at,
  UNIQUE(provider, location_reference)

replication_jobs
  id, source_replica_id, target_provider, target_storage_class,
  state, attempt_count, next_attempt_at, error_code, error_detail,
  started_at, completed_at
```

`location_reference` là opaque string: R2 dùng object key, Google Drive dùng file ID. `provider_metadata` chỉ là dữ liệu adapter cần (ví dụ Drive/shared-drive ID), không serialize ra public API. Nếu cần chuyển đổi an toàn, giữ `media.object_key` tạm thời, backfill sang R2 `storage_replicas`, rồi xóa cột sau khi tất cả read/write đã đi qua replica repository.

## 4. Ports và adapters

Core `media` context chỉ định nghĩa capability chung, không định nghĩa theo R2/Drive:

```go
// Minh họa contract, chưa phải mã triển khai.
type BlobStorage interface {
    CreateUploadTarget(ctx context.Context, in CreateUploadTargetInput) (UploadTarget, error)
    CreateReadTarget(ctx context.Context, ref LocationReference, ttl time.Duration) (ReadTarget, error)
    Stat(ctx context.Context, ref LocationReference) (ObjectInfo, error)
    OpenRead(ctx context.Context, ref LocationReference) (io.ReadCloser, ObjectInfo, error)
    Put(ctx context.Context, in PutInput) (LocationReference, ObjectInfo, error)
    Delete(ctx context.Context, ref LocationReference) error
}
```

- `R2BlobStorage`: presigned browser PUT và primary delivery ban đầu.
- `GoogleDriveBlobStorage`: backup/restore trong phase đầu; worker stream dữ liệu vào adapter và nhận opaque Drive file reference.
- Future S3/B2/Azure adapters không thay đổi media use case hay frontend.
- `MediaRepository`, `ReplicaRepository`, `ReplicationJobRepository` là ports; `ReplicaSelector` là policy thuần chọn replica `verified` theo availability/cost/lifecycle.
- Provider SDK types, OAuth, quota và error schema bị cô lập trong `internal/platform/<provider>` như một anti-corruption layer.

Không tạo một `StorageProvider` khổng lồ mang pricing, Drive OAuth, R2 presign và delivery. Core chỉ biết location reference, metadata và replica state.

## 5. Flows

### Upload P0

```text
Browser → request target → R2 direct PUT → complete
        → R2 HeadObject verify → Media ready
        → persist original R2 primary replica (verified)
        → write MediaReady transactional-outbox event
```

Chỉ cấp direct upload target cho primary write provider. Không cấp Google Drive upload link cho guest ở P0.

### Weekly archive migration

```text
Weekly scheduler
  → selects R2 primary replicas eligible for archive
  → archive worker claims idempotent job
  → OpenRead verified R2 source (streaming; never whole video in RAM)
  → Google Drive Put
  → Stat/download verification: exact size + checksum
  → Drive storage replica = verified
  → change delivery/archive policy atomically
  → delete R2 copy
  → R2 storage replica = deleted; job = completed
```

- Job idempotency key: `(variant_id, target_provider, target_storage_class)`.
- Dùng exponential backoff + jitter; Drive/R2 outage chỉ làm job retry, không block upload/gallery.
- Không coi Drive archive complete chỉ vì provider trả success: phải xác minh size và checksum trước khi được phép xóa R2.
- Source bị moderated/deleted trước job thì cancel job; không tạo backup mới.
- Job phải có explicit archive cutoff (ví dụ media đã `ready` trước thời điểm weekly run) để không đụng file đang upload/processing.
- Không xóa R2 nếu Drive copy `failed`, `pending`, `copying`, metadata mismatch hoặc chưa có deletion authorization của job.

### Delivery

```text
authorized media request
  → resolve asset + variant
  → ReplicaSelector chooses verified, non-deleted delivery replica
  → short-lived signed redirect or controlled proxy response
```

Trong giai đoạn còn R2 copy, ưu tiên R2 cho web delivery. Sau archive migration có hai policy có thể cấu hình:

1. **Drive direct controlled delivery:** resolver tạo read target ngắn hạn qua Google Drive adapter. Chỉ chọn sau khi kiểm thử authorization, range request, latency và quota thực tế.
2. **Restore-on-read (khuyến nghị cho gallery):** resolver đưa job restore Drive → R2 cache; UI trả trạng thái `restoring`, sau đó URL logical cũ phục vụ từ R2 cache TTL. Cách này giữ Google Drive ở archive tier thay vì biến nó thành CDN.

Không trả Drive share link public trong bất kỳ policy nào.

## 6. Replication, retention và chi phí

| Lifecycle | Delivery | Backup | Mục tiêu |
|---|---|---|---|
| Upload đến weekly archive run | R2 hot | Chưa cần Drive copy | UX upload/gallery nhanh |
| Weekly archive job thành công | Google Drive verified archive | R2 bị xóa sau verification | tối ưu chi phí R2 |
| Archived media được xem lại | R2 temporary restore cache hoặc Drive controlled read | Google Drive giữ archive copy | cân bằng chi phí và UX |
| Delete/export | temporary restore nếu cần | tombstone/delete mọi replica | privacy và ownership |

Các ngưỡng chỉ là cấu hình policy, không hard-code. Pricing, egress, quota, SLA và compliance phải review trước khi bật migration tự động.

- `archived` chỉ đúng khi Drive replica `verified`, checksum khớp original và R2 deletion receipt đã được lưu.
- Không delete R2 khi job mới bắt đầu; thứ tự bất biến là **copy → verify → atomically update replica state/routing → delete R2 → record deletion receipt**.
- Sau khi xóa R2, chỉ còn một Google Drive copy. Điều này tối ưu chi phí nhưng không đạt mục tiêu multi-copy durability/3-2-1; phải chấp nhận như một business decision hoặc thêm backup provider sau này.
- Restore-to-R2 cache là copy mới, có TTL và cleanup job; không được trở thành permanent unmanaged copy.
- Có thể deduplicate physical blob theo checksum trong cùng encryption/retention boundary; P0 không cross-event dedup để tránh xung đột quyền xóa/privacy.

## 7. Google Drive adapter và vận hành

1. Google Drive archive là **Drive cá nhân của chủ hệ thống**. Server kết nối qua OAuth consent một lần; refresh token được inject từ secret manager/environment, không lưu ở database client hay source code.
2. Lưu Drive file ID vào `storage_replicas.location_reference`; không lưu/public Drive share link.
3. Adapter tạo folder convention, upload/download stream, stat, delete, map lỗi provider sang lỗi nội bộ.
4. OAuth client credentials, refresh token, scopes, rotation, quota/backoff và provider schema chỉ có trong adapter.
5. File backup không public-share; CandidCrowd API quyết định mọi guest/host access.
6. Test restore drill định kỳ từ Drive sang replica mới, không chỉ check file ID tồn tại.

Trước implementation phải chốt Google account ownership/retention, OAuth consent/refresh-token rotation, quota/giới hạn file thực tế, region/compliance và recovery procedure nếu account bị mất quyền. Đây là quyết định vận hành có chủ đích, không ngầm định trong code.

### Environment configuration

Không hard-code provider credentials, Drive IDs, folder IDs, retention/replication thresholds hay routing policy. Chúng được nạp qua environment/configuration và validate lúc startup/worker boot:

```text
MEDIA_HOT_PROVIDER=r2
MEDIA_ARCHIVE_PROVIDER=google_drive
MEDIA_DELIVERY_PROVIDER_ORDER=r2,google_drive
MEDIA_DAILY_ARCHIVE_ENABLED=false
MEDIA_DAILY_ARCHIVE_SCHEDULE=0 2 * * *
MEDIA_ARCHIVE_MIN_AGE=168h
MEDIA_ARCHIVE_MAX_ATTEMPTS=8
MEDIA_RESTORE_ON_READ_ENABLED=true
MEDIA_RESTORE_CACHE_TTL=24h

GOOGLE_DRIVE_AUTH_MODE=oauth_refresh_token
GOOGLE_DRIVE_CLIENT_ID=...
GOOGLE_DRIVE_CLIENT_SECRET=...
GOOGLE_DRIVE_REFRESH_TOKEN=...               # secret manager/env injection only
GOOGLE_DRIVE_ROOT_FOLDER_ID=...
```

- `.env.example` chỉ có key/tên biến và placeholder vô hại; tuyệt đối không commit giá trị thật, service-account JSON hay Drive IDs nhạy cảm.
- Production inject secrets từ secret manager/deployment platform; local development dùng `.env` bị gitignore.
- `provider`, `location_reference`, bucket/file IDs của từng replica là runtime data trong database, không phải constants trong code và không nằm trong response client.
- R2 config thiếu hoặc sai: hot provider phải fail fast lúc boot. Google Drive config thiếu/sai: weekly archive worker phải disabled rõ ràng và alert, không âm thầm xóa R2 hay fallback/hard-code provider.

## 8. Delete, privacy và restore

```text
logical media deleted
  → revoke delivery ngay
  → mark replicas deleting
  → provider delete mỗi replica với retry
  → audit deletion receipt
  → replicas deleted hoặc deletion_failed alert
```

- Xóa R2 không đủ nếu copy Drive còn tồn tại.
- Retention/legal hold (nếu cần sau này) phải là policy explicit, audit được, không silent bypass privacy.
- Restore tạo replica mới từ bản `verified`, giữ logical `media_id`, audit actor/reason/source.

## 9. Observability và kiểm thử

Theo dõi theo provider/variant mà không log URL/secrets:

- replication lag (`ready_at → backup verified_at`), verified/failed/retry rate;
- media chưa có external verified replica, checksum mismatch, restore-drill success rate;
- storage bytes theo provider/tier/event age; delivery fallback/provider error rate;
- deletion completion lag across replicas.

Test bắt buộc:

- Unit tests với in-memory storage/replica/job ports và `ReplicaSelector` policy.
- Integration tests: R2 source → fake Drive adapter; retry worker crash; duplicate event; checksum mismatch; delete-before-copy.
- Restore drill: đọc được original từ backup và verify checksum.
- API/FE test: provider details không xuất hiện trong JSON, React Query cache, logs hoặc persisted client state.

## 10. Phases triển khai

### P0 — Portable foundation, chỉ R2

1. Logical media URL/IDs trong FE và API; bỏ object key/provider URL khỏi public responses.
2. Thêm `media_variants`, `storage_replicas`; new upload tạo R2 primary replica verified.
3. Refactor key-centric `Storage` thành location-reference-aware blob capability; thêm repository ports.
4. `ReplicaSelector` luôn chọn R2; add transactional outbox + replication job schema nhưng chưa bật Drive.

### P1 — Weekly Google Drive archive migration

1. Thực hiện OAuth consent cho Google Drive cá nhân, inject client credentials/refresh token qua secrets và có quy trình rotation/revoke.
2. Implement adapter, mock adapter, weekly scheduler và R2 → Drive streaming archive worker.
3. Archive original trước; thumbnails/derived variants chỉ sau khi metric chứng minh cần thiết.
4. Chỉ xóa R2 sau Drive checksum verification + persisted archive state; lưu deletion receipt/audit.
5. Enable retry/backoff, dashboards, alert, restore-on-read cache và restore drill.

### P2 — Migration/tiering đa provider

1. Add provider adapter mới, không đổi frontend/public API.
2. Copy → verify → change `ReplicaSelector` policy → safety window → delete source.
3. Lifecycle automation theo age, access frequency, plan và backup status.

## 11. Acceptance criteria

- [ ] Không FE/public API persistence nào phụ thuộc R2 key/bucket, Drive file ID hoặc provider URL.
- [ ] Weekly R2 → Drive failure không chặn upload, complete hay gallery, và không xóa R2 source.
- [ ] Archive retry idempotent; worker crash không tạo duplicate Drive file.
- [ ] Drive archive chỉ `verified` khi size/checksum khớp; R2 chỉ bị xóa sau đó.
- [ ] Delete/moderation revoke delivery ngay và theo dõi xóa hết replicas.
- [ ] Restore drill đọc lại được media từ Google Drive và phục vụ lại qua logical URL cũ.
- [ ] Migration provider không đổi link/ID FE hoặc public API.

## 12. Rủi ro chấp nhận rõ

- Google Drive không phải CDN/object-storage delivery contract; ưu tiên restore-on-read cho gallery sau khi R2 bị archive.
- Việc xóa R2 sau weekly migration giảm chi phí nhưng để archive chỉ còn một Drive copy; mất quyền Google account hoặc lỗi xóa nhầm có thể gây mất dữ liệu.
- Archive tuần một lần đồng nghĩa file mới nhất có thể chưa có copy Drive trước lần job kế tiếp; RPO/RTO phải được chấp nhận và đo bằng restore drill.
