# Worker Transcode

Permanent S3 upload failures fall back to Temp → Local. Separated video/audio ingests preserve track metadata for worker-transfer.

Queue-based transcode worker สำหรับ [VdoHide](https://vdohide.xyz) — แปลงวิดีโอเป็นหลาย resolution (360/480/720/1080) แล้วส่งเข้าท่อ ingest ให้ worker-transfer ติดตั้งลง storage

> แทนที่ `server-transcode` เดิมที่ scan หาไฟล์เอง — ตัวนี้รับงานจากคิวอย่างเดียว **ไม่ผูก storage**: เครื่องไหนก็รันได้ (โหลด original ผ่าน storage-node หรือ S3 `originUrl` → encode → permanent S3; fallback S3 temp)

## Features

- **Pool กลาง** — claim งาน `transcode` จาก `video_process` ตัวไหนว่างก็หยิบ ไม่มี targetStorageId
- **S3 native** — original บน S3 โหลดผ่าน `originUrl`; rendition อัพขึ้น permanent S3 และสร้าง Media/Clone ทันที
- **Dynamic resolutions** — 360 เสมอ + 480/720/1080 ตาม short side ของต้นฉบับ (YouTube-style 95% tolerance เช่น 478px ก็ได้ 480p) ข้าม resolution ที่มี media/ingest อยู่แล้ว
- **GPU auto-detect** — NVIDIA nvenc → AMD amf → Intel qsv → CPU (libx264) | encode fail บน GPU → fallback CPU อัตโนมัติ | ปิด GPU ได้ผ่าน setting `transcode_config.gpuEnabled`
- **Adaptive fanout** — GPU + วิดีโอไม่เกิน 30 นาที decode ครั้งเดียวแล้ว encode ทุก resolution พร้อมกัน; วิดีโอยาว 720p+ ทำ 360p ให้พร้อมก่อนแล้ว fanout ที่เหลือ; CPU ทำทีละ resolution เสมอ
- **Dynamic bitrate capping** — cap ตาม bitrate ต้นฉบับ (สัดส่วน pixel² × 0.85) ไฟล์ output ไม่บวมเกินต้นฉบับ
- **Retry resume** — fail แล้ว retry ใช้ของเดิมต่อ: original ที่โหลดค้าง (เช็คขนาดกับ metadata) + resolution ที่ encode เสร็จแล้วไม่ทำซ้ำ
- **Instant Cancel** — admin เซ็ต `status: cancelled` → context ยกเลิก → **ffmpeg โดน kill ทันที** (CommandContext) + เก็บกวาด temp
- **Auto Retry + Backoff** — fail → pending ใน doc เดิม (1m, 2m) ครบ 3 ครั้ง → failed ถาวร (ไฟล์ยังเล่นได้จาก original)
- **Progress จริง** — เขียน % ลง DB แบบ throttle ทุก ~5% (download / encode_{res} / upload_{res}) — งาน encode ยาวเป็นชั่วโมง หน้า admin เห็นความคืบหน้า
- **จบงาน** — อัปเดต `file.metadata.highest` เป็น resolution สูงสุด + กระจายไป cloned files (ป้ายคุณภาพให้ UI — enqueuer ตัดสิน "เคย transcode หรือยัง" จาก media/ingest จริง ไม่ใช่ field นี้)
- **Log per job** — เก็บ `logs/process/<slug>.log` ในเครื่อง 7 วันแล้วลบอัตโนมัติ (ไม่อัพโหลดขึ้น S3) เปิดดูได้ที่ `/log/<slug>.log`
- **Live process log** — กด `View log` ใน Dashboard เพื่ออ่าน log เฉพาะเมื่อเปิด (refresh ทุก 1 วินาที, สูงสุดท้ายไฟล์ 512 KiB)

## Requirements

- **MongoDB** (vdohide platform database)
- **vdohide-service** รันอยู่ (enqueuer `getTranscodePending` เติมคิว + reaper)
- **ffmpeg + ffprobe** (install.sh ติดตั้งให้) — GPU ต้องมี driver + ffmpeg ที่มี nvenc
- ไฟล์ต้อง `ready` แล้ว (Local ต้องเข้าถึง storage-node; S3 ต้องมี `originUrl`)

---

## Installation (Linux Server)

สำหรับ Runpod GPU Pod ดูคู่มือแบบครบตั้งแต่เลือก GPU, สร้าง Template, ตั้ง Secret,
เปิดหลาย worker และตรวจ NVENC ที่ [INSTALL_RUNPOD.md](INSTALL_RUNPOD.md)

```bash
curl -fsSL https://raw.githubusercontent.com/zergolf1994/worker-transcode/main/install.sh | sudo -E bash -s -- \
    --database-url "mongodb+srv://user:pass@cluster.mongodb.net/platform" \
    --media-layout separated \
    --pipeline-mode adaptive \
    --fanout-max-minutes 30 \
    --upload-overlap true \
    --max-parallel-uploads 2 \
    -n 1
```

| Option | Default | คำอธิบาย |
|---|---|---|
| `-n, -w, --count` | `1` | จำนวน worker instances; adaptive GPU ควรเริ่ม 1 แล้ว benchmark ก่อนเพิ่มเป็น 2 |
| `--database-url` | `""` | MongoDB connection string (`DATABASE_URL`) |
| `--media-layout` | `muxed` | `muxed` หรือ `separated` |
| `--pipeline-mode` | `adaptive` | `adaptive`, `fanout` หรือ `sequential` |
| `--fanout-max-minutes` | `30` | threshold ของ adaptive fanout (`1–1440` นาที) |
| `--upload-overlap` | `true` | encode ตัวถัดไประหว่าง upload (`true`/`false`) |
| `--max-parallel-uploads` | `2` | จำนวน background uploads (`1–4`) |
| `--uninstall` | — | ถอนการติดตั้ง |

```bash
journalctl -u "worker-transcode@*" -f              # ดู logs
for i in $(seq 1 2); do systemctl restart worker-transcode@$i; done
```

## ทดสอบ 2 Workers บน Windows

หลังจากรัน `build.bat` แล้ว เปิด Terminal จากโฟลเดอร์ repository:

```bat
.\run-2-workers.cmd
```

สคริปต์จะเปิด `.build\windows.exe` เป็น worker `@1` และ `@2` โดย Dashboard ใช้
`http://localhost:8886` ร่วมกัน กด `Ctrl+C` เพื่อหยุดทั้งสอง process

ถ้าใช้ PowerShell โดยตรงสามารถรัน `.\run-2-workers.ps1` ได้ ส่วน Git Bash ให้ใช้
`./run-2-workers.cmd`

## Configuration (.env)

```env
DATABASE_URL=mongodb+srv://user:pass@cluster.mongodb.net/platform

# Optional
WORKER_ID=transcode_myhost@1
REDIS_URL=redis://localhost:6379/0
S3_UPLOAD_CONCURRENCY=2
MEDIA_LAYOUT=muxed # muxed | separated
TRANSCODE_PIPELINE_MODE=adaptive # adaptive | fanout | sequential
TRANSCODE_FANOUT_MAX_MINUTES=30
TRANSCODE_UPLOAD_OVERLAP=true
TRANSCODE_MAX_PARALLEL_UPLOADS=2
```

### การตั้งค่า Transcode Pipeline

| Environment | Default | ค่าที่รองรับ | ใช้ทำอะไร |
|---|---:|---|---|
| `TRANSCODE_PIPELINE_MODE` | `adaptive` | `adaptive`, `fanout`, `sequential` | เลือกวิธีจัดลำดับ encode/upload |
| `TRANSCODE_FANOUT_MAX_MINUTES` | `30` | `1–1440` นาที | threshold แบ่งไฟล์สั้น/ยาวในโหมด adaptive |
| `TRANSCODE_UPLOAD_OVERLAP` | `true` | `true`, `false` | ให้ CPU encode ตัวถัดไประหว่าง upload ตัวก่อนหน้า |
| `TRANSCODE_MAX_PARALLEL_UPLOADS` | `2` | `1–4` | จำกัดจำนวน background uploads ใน sequential/fallback pipeline |

#### `TRANSCODE_PIPELINE_MODE`

- `adaptive` (แนะนำ): GPU + ไฟล์ไม่เกิน threshold จะ decode ครั้งเดียวแล้ว fanout ทุก
  resolution; ไฟล์ยาวที่มี 720p ขึ้นไปจะ encode/upload 360p ก่อนแล้ว fanout ที่เหลือ;
  ไฟล์ยาวที่สูงสุดไม่เกิน 480p จะ fanout 360p/480p; CPU encode ทีละ resolution เสมอ
- `fanout`: GPU decode ครั้งเดียวแล้ว encode ทุก resolution พร้อมกันโดยไม่สนความยาว
  ถ้าไม่มี GPU หรือ GPU ล้มเหลวจะ fallback เป็น CPU sequential
- `sequential`: โหมดเดิม `encode → upload → resolution ถัดไป` ไม่ใช้ fanout และไม่
  เปิด upload overlap เหมาะสำหรับ rollback

#### `TRANSCODE_FANOUT_MAX_MINUTES`

ใช้เฉพาะ `adaptive` โดยค่าที่เท่ากับ threshold ยังถือเป็นไฟล์สั้น เช่น ค่า `30` หมายถึง
ไฟล์ยาวไม่เกิน 30 นาที fanout ทุก resolution ส่วนไฟล์ที่ยาวกว่า 30 นาทีและมี 720p
ขึ้นไปจะทำ 360p ให้พร้อมก่อน ค่าแนะนำทั่วไปคือ `30`

#### `TRANSCODE_UPLOAD_OVERLAP`

เมื่อเป็น `true` และ adaptive ต้อง fallback มาใช้ CPU ระบบยังเปิด FFmpeg encode เพียง
หนึ่งตัว แต่จะ upload output ที่เสร็จแล้วเบื้องหลังพร้อมกับ encode resolution ถัดไป
ถ้าเป็น `false` จะรอ upload ก่อนเริ่ม encode ตัวถัดไป โหมด explicit `sequential`
จะไม่ใช้ overlap แม้ตั้งค่านี้เป็น `true`

GPU `priority360` จะ upload 360p พร้อมกับ fanout 480p/720p/1080p ตามการออกแบบของ
pipeline และจะยังไม่ publish คุณภาพสูงจนกว่า 360p จะสำเร็จ

#### `TRANSCODE_MAX_PARALLEL_UPLOADS`

จำกัดจำนวน output files ที่ upload เบื้องหลังพร้อมกันใน CPU sequential/fallback path
ค่า `1` ใช้ network/RAM ต่ำสุด, `2` เป็นค่าที่แนะนำ และ `3–4` เหมาะเมื่อ S3/network
เร็วพอ ค่านี้ต่างจาก `S3_UPLOAD_CONCURRENCY` ซึ่งเป็นจำนวน multipart parts ที่ upload
พร้อมกัน **ภายในไฟล์เดียว**

ค่าที่แนะนำทั่วไป:

```env
TRANSCODE_PIPELINE_MODE=adaptive
TRANSCODE_FANOUT_MAX_MINUTES=30
TRANSCODE_UPLOAD_OVERLAP=true
TRANSCODE_MAX_PARALLEL_UPLOADS=2
```

ค่าสำหรับ rollback กลับไปใช้ flow เดิม:

```env
TRANSCODE_PIPELINE_MODE=sequential
TRANSCODE_UPLOAD_OVERLAP=false
TRANSCODE_MAX_PARALLEL_UPLOADS=1
```

## Settings ใน DB (collection `settings`)

| name | ใช้ทำอะไร |
|---|---|
| `transcode_config` | `{enabled, slotRate, gpuEnabled}` — kill switch + ขนาดคิว + อนุญาต GPU |

---

## Transcode Flow (1 job = 1 file)

1. **download (10%)** — Local โหลดจาก storage-node; S3 โหลดจาก `https://{originUrl}/{fileId}/{fileName}` — cache ไว้ retry
2. **probe** — ขนาด/ความยาว/bitrate → คำนวณ resolutions เป้าหมาย; เมื่อ `MEDIA_LAYOUT=separated` จะดึง audio ที่ยังฝังใน original เป็น `{fileId}/audio_N.m4a` ก่อน
3. **encode_{res} + upload_{res} (85%)** — adaptive GPU fanout ตามความยาว/คุณภาพ หรือ sequential เมื่อใช้ CPU; `muxed` จะคงเสียง AAC ใน MP4 แบบเดิม ส่วน `separated` จะสร้าง MP4 แบบ video-only → permanent S3 (`{fileId}/file_{res}.mp4`) → Media/Clone → ลบ local output ทันที; ถ้าไม่มีหรืออัพไม่สำเร็จ จึง fallback S3 temp + ingest ให้ worker-transfer ติดตั้ง
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
| `storages` | Local storage-node/S3 origin ต้นทาง, permanent S3 หรือ S3 temp ปลายทาง |
| `settings` | `transcode_config` |

> ⚠ **Index ทั้งหมดเป็นของฝั่ง vdohide-service (mongoose)** — repo นี้ไม่สร้าง index เอง
