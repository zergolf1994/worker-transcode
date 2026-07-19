# Worker Transcode

Queue-based transcode worker สำหรับ [VdoHide](https://vdohide.xyz) — แปลงวิดีโอเป็นหลาย resolution (360/480/720/1080) แล้วส่งเข้าท่อ ingest ให้ worker-transfer ติดตั้งลง storage

> แทนที่ `server-transcode` เดิมที่ scan หาไฟล์เอง — ตัวนี้รับงานจากคิวอย่างเดียว **ไม่ผูก storage**: เครื่องไหนก็รันได้ (โหลด original ผ่าน HTTP จาก storage-node → encode → อัพ S3 temp)

## Features

- **Pool กลาง** — claim งาน `transcode` จาก `video_process` ตัวไหนว่างก็หยิบ ไม่มี targetStorageId
- **Dynamic resolutions** — 360 เสมอ + 480/720/1080 ตาม short side ของต้นฉบับ (YouTube-style 95% tolerance เช่น 478px ก็ได้ 480p) ข้าม resolution ที่มี media/ingest อยู่แล้ว
- **GPU auto-detect** — NVIDIA nvenc → AMD amf → Intel qsv → CPU (libx264) | encode fail บน GPU → fallback CPU อัตโนมัติ | ปิด GPU ได้ผ่าน setting `transcode_config.gpuEnabled`
- **Dynamic bitrate capping** — cap ตาม bitrate ต้นฉบับ (สัดส่วน pixel² × 0.85) ไฟล์ output ไม่บวมเกินต้นฉบับ
- **Retry resume** — fail แล้ว retry ใช้ของเดิมต่อ: original ที่โหลดค้าง (เช็คขนาดกับ metadata) + resolution ที่ encode เสร็จแล้วไม่ทำซ้ำ
- **Instant Cancel** — admin เซ็ต `status: cancelled` → context ยกเลิก → **ffmpeg โดน kill ทันที** (CommandContext) + เก็บกวาด temp
- **Auto Retry + Backoff** — fail → pending ใน doc เดิม (1m, 2m) ครบ 3 ครั้ง → failed ถาวร (ไฟล์ยังเล่นได้จาก original)
- **Progress จริง** — เขียน % ลง DB แบบ throttle ทุก ~5% (download / encode_{res} / upload_{res}) — งาน encode ยาวเป็นชั่วโมง หน้า admin เห็นความคืบหน้า
- **จบงาน** — อัปเดต `file.metadata.highest` เป็น resolution สูงสุด + กระจายไป cloned files (ป้ายคุณภาพให้ UI — enqueuer ตัดสิน "เคย transcode หรือยัง" จาก media/ingest จริง ไม่ใช่ field นี้)
- **Log per job** — จบงาน → อัพ `logs/process/<slug>.log` ขึ้น S3 ที่ `logs/transcode/`

## Requirements

- **MongoDB** (vdohide platform database)
- **vdohide-service** รันอยู่ (enqueuer `getTranscodePending` เติมคิว + reaper)
- **ffmpeg + ffprobe** (install.sh ติดตั้งให้) — GPU ต้องมี driver + ffmpeg ที่มี nvenc
- ไฟล์ต้อง `ready` แล้ว (original อยู่บน storage-node — โหลดผ่าน HTTP)

---

## Installation (Linux Server)

```bash
curl -fsSL https://raw.githubusercontent.com/zergolf1994/worker-transcode/main/install.sh | sudo -E bash -s -- \
    --database-url "mongodb+srv://user:pass@cluster.mongodb.net/platform" \
    -n 1
```

| Option | Default | คำอธิบาย |
|---|---|---|
| `-n, -w, --count` | `1` | จำนวน worker instances (CPU: 1 แนะนำ / GPU: หลายตัวได้) |
| `--database-url` | `""` | MongoDB connection string (`DATABASE_URL`) |
| `--uninstall` | — | ถอนการติดตั้ง |

```bash
journalctl -u "worker-transcode@*" -f              # ดู logs
for i in $(seq 1 2); do systemctl restart worker-transcode@$i; done
```

## Configuration (.env)

```env
DATABASE_URL=mongodb+srv://user:pass@cluster.mongodb.net/platform

# Optional
WORKER_ID=transcode_myhost@1
LOG_PATH=logs/worker-transcode.log
```

## Settings ใน DB (collection `settings`)

| name | ใช้ทำอะไร |
|---|---|
| `transcode_config` | `{enabled, slotRate, gpuEnabled}` — kill switch + ขนาดคิว + อนุญาต GPU |

---

## Transcode Flow (1 job = 1 file)

1. **download (10%)** — โหลด original จาก storage-node (`http://{host}/{mediaSlug}.mp4`) — cache ไว้ retry
2. **probe** — ขนาด/ความยาว/bitrate → คำนวณ resolutions เป้าหมาย
3. **encode_{res} + upload_{res} (85%)** — ทีละ resolution: ffmpeg → อัพ S3 temp (key มีวันที่ `{date}/{fileId}_file_{res}.mp4`) → สร้าง ingest `processed` → **worker-transfer เห็น ingest แล้วติดตั้ง + สร้าง media เอง** (partial ได้ — 360 เสร็จก่อนก็ดูได้ก่อน)
4. **finish (100%)** — `file.metadata.highest` + clones

```
vdohide-service                worker-transcode (ตัวนี้)         worker-transfer
enqueuer:transcode ──pending──▶ claim → download → encode ─┐
  ไฟล์ ready ที่ยังไม่มี media                              │ S3 + ingest
  360-1080 / ingest ค้าง       ◀──ประทับ highest จบงาน      ▼
                                                      เห็น ingest → โหลด → ติดตั้ง
                                                      → media record → purge CF
```

## Collections Used

| Collection | การใช้งาน |
|---|---|
| `video_process` | คิวงาน — claim/settle/timeline (+% realtime) |
| `workers` | heartbeat, สถานะ, system info |
| `files` | metadata.highest (marker จบงาน) |
| `medias` | หา original + เช็ค resolution ที่มีแล้ว |
| `ingests` | สร้าง ingest processed ให้ worker-transfer |
| `storages` | storage-node ต้นทาง (HTTP), S3 temp ปลายทาง |
| `settings` | `transcode_config` |

> ⚠ **Index ทั้งหมดเป็นของฝั่ง vdohide-service (mongoose)** — repo นี้ไม่สร้าง index เอง
