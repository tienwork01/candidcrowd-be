# Runbook: gói sự kiện (event plans)

**Đối tượng:** người vận hành CandidCrowd (QA, hỗ trợ, on-call).  
**Liên quan:** kế hoạch triển khai `candid-crowd/docs/plans/event-plans-implementation-plan.md`.

Mọi thay đổi gói đều đi qua `cmd/planctl`, ghi audit không sửa được và không có HTTP endpoint công khai nào để cấp gói. Lệnh cần `DATABASE_URL`; nếu có `REDIS_URL`, màn hình host đang mở sẽ tự cập nhật khi gói đổi.

```bash
go build -o planctl ./cmd/planctl
```

## 1. Xem trạng thái một event

```bash
planctl show --event <event-id>
```

In gói hiện tại, quyền lợi, hạn nhận upload, hạn lưu trữ, mức dùng so với giới hạn và lịch sử đổi gói (mới nhất trước).

## 2. Cấp gói thủ công

```bash
planctl grant --event <event-id> --plan experience|signature \
  --source-ref <mã-ticket-duy-nhất> --reason "<lý do>"
```

- `--source-ref` là khóa chống cấp trùng: cùng một mã chỉ cấp được một lần, chạy lại sẽ báo lỗi và không đổi gì. Dùng mã ticket hỗ trợ hoặc mã giao dịch bên ngoài.
- Chỉ nâng cấp: `free → experience → signature`. Hạ gói bị từ chối.
- Tên user của máy chạy lệnh được ghi kèm `reason` trong audit.
- `--source promotion` cho khuyến mại; mặc định là `manual`.
- Hạn lưu trữ mới không bao giờ ngắn hơn hạn cũ.

## 3. Thu hồi gói cấp nhầm

```bash
planctl revoke --event <event-id> --reason "<lý do>"
```

- Grant hiện tại chuyển sang `revoked`; grant ngay trước đó được khôi phục cùng giới hạn và hạn lưu trữ của nó.
- Không thu hồi được grant đầu tiên của event (event luôn phải có gói).
- Media đã upload được giữ nguyên. Nếu event đã vượt giới hạn của gói cũ, upload mới sẽ bị từ chối cho đến khi được nâng cấp lại.
- `--source-ref` của grant đã thu hồi vẫn bị coi là đã dùng. Khi cấp lại, dùng mã mới.

## 4. Backfill event cũ

```bash
planctl backfill-legacy --created-before <RFC3339> --dry-run
planctl backfill-legacy --created-before <RFC3339>
```

Chạy hai lần theo plan (§5.5): ngay sau migration và ngay trước khi bật `ENTITLEMENT_ENFORCEMENT_ENABLED`, với mốc là thời điểm bật. Chạy lại an toàn.

## 5. Đối soát

```bash
planctl reconcile
```

| Dòng | Ý nghĩa | Nếu khác 0 |
| ---- | ------- | ---------- |
| events without an active plan | Event không có grant active | Chạy `backfill-legacy` lần nữa hoặc kiểm tra event tạo lỗi |
| limits differ from the plan | Giới hạn trên event lệch snapshot của grant | Báo dev; tạm thời cấp lại cùng gói không được (chỉ nâng cấp) |
| counters differ from media | Bộ đếm upload/byte lệch dữ liệu media | Báo dev kèm danh sách event |
| due for retention purge | Đã qua hạn lưu trữ + ân hạn, chưa xóa | Bình thường nếu retention đang tắt |
| storage ending soon | Hạn lưu trữ kết thúc trong 7 ngày | Thông tin; dùng để báo host khi có hệ thống thông báo |

Lệnh trả exit code khác 0 khi có vi phạm ở 3 dòng đầu, nên có thể đặt chạy định kỳ và cảnh báo theo exit code.

## 6. Xóa media hết hạn (retention)

Đây là **xóa vĩnh viễn**: ảnh gốc, thumbnail, file ZIP export, logo QR trong R2 (`events/<id>/`) và bản sao trên Google Drive.

Điều kiện: grant active có `retention_expires_at + RETENTION_GRACE` (mặc định 30 ngày) đã qua và event chưa bị xóa trước đó.

```bash
planctl retention            # chỉ liệt kê, không xóa
planctl retention --execute  # xóa thật (cần biến R2_* và, nếu có archive, GOOGLE_DRIVE_*)
```

- Job tự động chạy hằng ngày trong worker (`RETENTION_SCHEDULE`, mặc định `30 3 * * *`). Khi `RETENTION_ENFORCEMENT_ENABLED=false`, job chỉ ghi log `plan.retention_due`.
- Thứ tự an toàn: xóa bản Drive → xóa object R2 → đánh dấu DB. Lỗi ở bước nào thì event không bị đánh dấu và được thử lại ở lần chạy sau. Nếu event có bản Drive mà không cấu hình Drive, event bị bỏ qua.
- Sau khi xóa: event chuyển `closed` (khách không còn truy cập), media đánh dấu `deleted`, export đánh dấu `failed`. Lịch sử và bộ đếm lượt upload được giữ.
- **Không có cách khôi phục** sau `--execute` (trừ bản sao lưu cấp hạ tầng nếu có). Luôn chạy bản liệt kê trước.

## 7. Theo dõi

Log có cấu trúc (JSON) cần theo dõi:

| Log `msg` | Ý nghĩa | Hành động |
| --------- | ------- | --------- |
| `plan.entitlement_denied` | Host dùng tính năng ngoài gói. `shadow=true` nghĩa là chưa chặn. | Trong giai đoạn shadow: so với kỳ vọng trước khi bật enforcement. Tăng đột biến sau khi bật: kiểm tra từ chối sai. |
| `plan.quota_reached` | Upload bị từ chối vì hết giới hạn (`resource`, `usage`, `limit`) | Theo dõi tỷ lệ theo gói; event Experience/Signature chạm giới hạn là bất thường |
| `plan.upload_window_closed` | Upload sau hạn nhận upload | Bình thường với event cũ |
| `plan.grant_missing` | Event không có grant khi kiểm tra quyền | Chạy `planctl reconcile` |
| `plan.retention_due` / `plan.retention_purged` / `plan.retention_purge_failed` | Retention | Lỗi lặp lại nhiều ngày cho cùng event cần xem tay |

## 8. Cờ triển khai

| Biến | Mặc định | Tác dụng |
| ---- | -------- | -------- |
| `PLANS_CATALOG_ENABLED` | `true` | Mở `GET /api/v1/plans` |
| `PLANS_PRICING_UI_ENABLED` | `false` | Homepage hiện pricing theo catalog thay cho Early Access |
| `MANUAL_PLAN_ACTIVATION_ENABLED` | `false` | Hiện card trả phí và hướng dẫn liên hệ |
| `ENTITLEMENT_ENFORCEMENT_ENABLED` | `false` | Chặn thật theo gói; tắt = shadow mode, upload theo `EVENT_MAX_MEDIA_BYTES` cũ |
| `RETENTION_ENFORCEMENT_ENABLED` | `false` | Job hằng ngày xóa thật media hết hạn |

Rollback: tắt pricing và manual activation trước; có thể đưa enforcement về shadow. Không thu hồi grant đã cấp chỉ vì rollback ứng dụng.

## 9. Thanh toán (billing)

### 9.1 Giá do backend quyết

`GET /api/v1/events/:id/offers` trả báo giá riêng cho từng host + event. Đây là nguồn sự thật duy nhất; trình duyệt không bao giờ gửi số tiền hay price ID.

| Tình huống | Base | Credit | Phải trả |
| ---------- | ---- | ------ | -------- |
| Mua lần đầu Experience | $39 | — | **$39** |
| Mua lần đầu Signature | $69 | — | **$69** |
| Khách quay lại (đã có purchase trả phí ở **event khác**) | $32 / $59 | — | **$32 / $59** |
| Nâng cấp event mua giá lần đầu | $69 | $39 | **$30** |
| Nâng cấp event mua giá quay lại | $59 | $32 | **$27** |
| Nâng cấp tier được tặng (không có purchase) | $69 | $39 | **$30** |
| Nâng cấp sau khi đã hoàn tiền | $69 | $0 | **$69** |

Hai quy tắc đứng sau bảng trên:

- **Credit là giá trị thương mại của tier, không phải tiền mặt đã trả.** Coupon giảm số tiền của chính lần mua đó, không làm tier rẻ đi khi nâng cấp.
- **Giá khách quay lại áp cho event *sau*, không áp cho chính event vừa mua.** Nâng cấp cùng một event luôn ở lại ngữ cảnh giá nó đã được bán.

### 9.2 Mapping với payment provider

```bash
planctl product-map list
planctl product-map set --plan experience --product-id pro_...
planctl price-map list        # tùy chọn, chỉ cho các mức giá chuẩn
```

Mỗi gói chỉ cần **một product**. Mọi số tiền (coupon, credit nâng cấp, khuyến mãi) được gửi sang Paddle dưới dạng non-catalog price, nên không phải tạo price mới cho từng mức tiền. `price-map` chỉ còn là tùy chọn: nếu có price khớp đúng số tiền thì dùng, không có thì tự động dùng product.

> Paddle từ chối tạo transaction nếu tài khoản chưa đặt **default payment link** (Checkout → Settings). Lỗi `transaction_default_checkout_url_not_set`. Biến `PADDLE_CHECKOUT_URL` chỉ ghi đè trang checkout cho từng transaction, **không thay thế** cấu hình này.

### 9.3 Hoàn tiền và chargeback

Nguyên tắc: **billing không bao giờ xóa event hay media**.

| Tình huống | License | Grant | Event |
| ---------- | ------- | ----- | ----- |
| Hoàn tiền, license chưa dùng | `revoked` | không có | `billing_status = ok` |
| Hoàn tiền toàn phần, license đã dùng | giữ `consumed` | **giữ nguyên** | `billing_status = refunded` |
| Hoàn tiền một phần | giữ | giữ | không đổi |
| Chargeback | giữ | **giữ nguyên** | `billing_status = payment_review` |
| Chargeback thắng kiện | giữ | giữ | về `ok`, purchase về `settled` |

```sql
-- Event đang chờ người xử lý
SELECT id, name, billing_status FROM events WHERE billing_status <> 'ok';
```

Hạ gói, chặn upload hay thu hồi quyền sau khi hoàn tiền là **quyết định của con người**, làm bằng `planctl revoke`, không tự động.

### 9.4 Đối soát và webhook

```bash
planctl billing-reconcile     # cấp lại entitlement cho purchase đã settle
planctl billing-show --purchase <id>
```

- Job tự chạy theo `BILLING_RECONCILE_SCHEDULE` (mặc định 5 phút/lần). Chạy bao nhiêu lần cũng chỉ tạo **đúng một license và một grant** cho mỗi purchase.
- Webhook trùng lặp bị chặn bằng `billing_provider_events (provider, external_event_id)`. Một lần giao thất bại sẽ được nhận lại ở lần provider thử lại.
- Thứ tự webhook không được tin: `transaction.completed` đến sau khi đã hoàn tiền sẽ bị bỏ qua, còn đến sau `payment_failed` thì vẫn settle.

| Log `msg` | Ý nghĩa | Hành động |
| --------- | ------- | --------- |
| `billing.webhook.unmatched` | Nhận webhook của transaction không có purchase | Kiểm tra transaction trong dashboard; provider sẽ thử lại |
| `billing.integrity_mismatch` | Số tiền/price trong webhook khác snapshot của purchase | **Điều tra ngay**, không cấp entitlement |
| `billing.webhook.settlement_after_reversal` | Thanh toán về sau khi đã hoàn tiền | Kiểm tra tay |
| `billing.event.flagged` | Event vào trạng thái review vì hoàn tiền/chargeback | Xử lý theo 9.3 |

### 9.5 Chuẩn bị trước khi bật `BILLING_ENABLED=true`

1. Paddle dashboard → Checkout → Settings → **Default payment link** = `https://<domain>/billing/checkout` (trang này đã có sẵn trong app, nó mở checkout từ `?_ptxn=`).
2. `planctl product-map set` cho cả `experience` và `signature`.
3. Khởi động API và kiểm tra log: không được có dòng `billing cannot charge for some plans`.
4. `planctl reconcile` — dòng `awaiting billing review` cho biết có event nào đang chờ xử lý refund/chargeback.

## 10. Thêm một gói mới

Gói **không** nằm trong code. Code chỉ hỏi registry: gói nào tồn tại, thứ tự nâng cấp ra sao, gói nào bán được. Thêm gói là thêm dữ liệu.

### 10.1 Các bước

```sql
-- 1. Đăng ký gói. tier_rank quyết định thứ tự nâng cấp (chỉ đi lên).
INSERT INTO plans (code, tier_rank, sellable) VALUES ('premier', 3, true);

-- 2. Phiên bản catalog: nó cho gì (features + limits).
INSERT INTO plan_versions (code, version, display_name, status, entitlements, activated_at)
VALUES ('premier', 1, 'Premier', 'active', '{"features":[...],"fair_use":true,"limits":{...}}', now());

-- 3. Giá niêm yết và giá khách quay lại, cùng một chỗ.
INSERT INTO plan_prices (plan_version_id, currency, amount_minor, returning_amount_minor, valid_from)
SELECT id, 'USD', 12900, 10900, now() FROM plan_versions WHERE code='premier' AND version=1;
```

```bash
# 4. Product bên payment provider.
planctl product-map set --plan premier --product-id pro_...

# 5. Khởi động lại API (registry nạp lúc boot) và kiểm tra log.
#    Phải thấy: plan registry loaded sellable=[experience signature premier]
#    Không được thấy: billing cannot charge for some plans
```

Hết. Không sửa file Go, không sửa file TypeScript, không migration mới.

### 10.2 Chèn gói vào giữa

`tier_rank` là số nguyên và duy nhất trong các gói còn sống, nên hãy chừa khoảng cách khi đặt (`10, 20, 30`) hoặc đánh lại số cho các gói phía trên. Chèn vào giữa sẽ tự động:

- đổi thứ tự `upgrade_options` của event
- đổi đường nâng cấp hợp lệ (`CheckTransition`)
- đổi thứ tự plan trong `GET /api/v1/plans`, và vì vậy đổi cả "gói rẻ nhất có tính năng này" trên UI

### 10.3 Ngừng bán nhưng không xóa

```sql
UPDATE plans SET sellable = false WHERE code = 'experience';
```

Gói biến mất khỏi báo giá và khỏi trang pricing, nhưng grant đã cấp vẫn chạy bình thường và event cũ không mất gì. Xóa hẳn gói thì dùng `retired_at`, và chỉ khi không còn `plan_versions` nào trỏ vào.

### 10.4 Những chỗ vẫn phải đụng tay (có chủ đích)

| Thứ | Vì sao không tự động |
| --- | -------------------- |
| `RECOMMENDED_PLAN` (frontend) | Quyết định marketing, không phải dữ liệu catalog |
| `.plan-badge--<code>` (CSS) | Không có style riêng thì badge dùng style trung tính, vẫn đọc được |
| Nội dung marketing của gói | Copywriting |

Tên hiển thị của gói lấy từ `plan_versions.display_name`, **không** dịch, nên không cần thêm key i18n.

### 9.6 Hai cấu hình Paddle hay nhầm

**Default payment link** (bắt buộc). `Checkout → Checkout settings` trong dashboard, trỏ về `https://<domain>/billing/checkout`. Không có thì Paddle từ chối tạo **mọi** transaction: `transaction_default_checkout_url_not_set`. Sandbox nhận `localhost`, production phải là domain đã duyệt.

**`PADDLE_CHECKOUT_URL`** (để trống). Biến này gửi `checkout.url` theo từng transaction, **ghi đè** default ở trên. Paddle chỉ chấp nhận domain đã duyệt ở `My account → Website approval`; đặt giá trị chưa duyệt — kể cả `localhost` — làm mọi checkout fail với `transaction_checkout_url_domain_is_not_approved`. Chỉ dùng khi một tài khoản Paddle phục vụ nhiều môi trường và các domain đều đã duyệt.

**Paddle không hỗ trợ idempotency key do client gửi.** Drill sandbox đã chứng minh: cùng một `Idempotency-Key`, cùng body, vẫn ra hai transaction khác nhau. Chống trùng nằm ở phía mình — mỗi purchase row gọi provider đúng một lần, và DB chỉ cho một purchase mở trên mỗi event.
