# ติดตั้ง Worker Transcode บน Runpod

คู่มือนี้อธิบายการนำ `worker-transcode` ไปรันเป็น GPU Pod บน Runpod ตั้งแต่เลือก
GPU, สร้าง Template, ตั้งค่า Secret, เปิดหลาย worker processes และตรวจว่า FFmpeg
กำลังใช้ NVENC จริง

> `worker-transcode` เป็น daemon ที่ poll คิวจาก MongoDB ตลอดเวลา จึงควรใช้
> **GPU Pod** ไม่ใช่ Runpod Serverless

## ค่าที่แนะนำสำหรับเริ่มต้น

ค่าตั้งต้นที่แนะนำสำหรับทดลอง adaptive pipeline:

| รายการ | ค่าเริ่มต้น |
|---|---|
| Cloud | Secure Cloud ถ้าต้องการความเสถียร หรือ Community Cloud ถ้าเน้นราคา |
| GPU | RTX 3090 หรือ RTX 4090 |
| GPU Count | **1 GPU ต่อ Pod** |
| Worker Count | `1` |
| Container Disk | `100 GB` |
| Network Volume | ไม่จำเป็นสำหรับการทดลอง |
| Image | `nvidia/cuda:12.8.1-runtime-ubuntu24.04` |
| HTTP Port | `8886/http` เฉพาะเมื่อต้องการเปิด Dashboard |
| Media Layout | `separated` |

เครื่อง Local GTX 1080 สามารถรันพร้อมกับ Runpod ได้ โดยให้ทั้งสองเครื่องเชื่อม
MongoDB ชุดเดียวกัน ระบบ queue จะกระจายงานให้เอง

## 1. เลือก GPU

โค้ดปัจจุบันใช้ `h264_nvenc` และรวมหลาย resolution ใน FFmpeg process เดียวเมื่อ
adaptive pipeline เลือก fanout แต่ขั้นตอน scale ภาพยังใช้ CPU ดังนั้นจำนวน vCPU
และความเร็ว network มีผลมาก ไม่ควรเลือกจาก CUDA cores เพียงอย่างเดียว

| GPU | จุดเด่น | Worker เริ่มต้น |
|---|---|---:|
| GTX 1080 Local | ใช้เป็นกำลังหลักที่ไม่มีค่าเช่ารายชั่วโมง | 1 |
| RTX 3090 | ค่าเช่าต่องานมักคุ้ม และบาง host ให้ CPU เยอะ | 1; ทดลอง 2 แล้วเทียบงานต่อชั่วโมง |
| RTX 4090 | สมดุลระหว่างราคาและ throughput | 1; ทดลอง 2 เมื่อ NVENC เฉลี่ยไม่เต็ม |
| RTX 5090 | throughput สูง แต่ adaptive fanout หนึ่งงานใช้หลาย encode streams | 1; ทดลอง 2 แล้ววัด throughput |
| RTX PRO 4500 Blackwell | เหมาะกับงานต่อเนื่องและหลาย encode sessions | 1; ทดลอง 2 หลังวัด CPU/NVENC |

เลือก region ที่อยู่ใกล้ **source storage และ S3 ปลายทาง** มากที่สุด การส่งไฟล์วิดีโอ
ข้ามทวีปอาจเสียเวลามากกว่าความเร็วที่ได้จากการเปลี่ยน GPU

### ทำไมใช้ GPU Count เท่ากับ 1

โปรแกรมยังไม่ได้กำหนด GPU index ให้แต่ละ worker process ถ้าเช่า Pod แบบ 2-4 GPUs
ทุก process อาจไปใช้ GPU 0 เป็นหลัก ทำให้ GPU ที่เหลือไม่ถูกใช้อย่างคุ้มค่า

ถ้าต้องการ capacity เพิ่ม ให้สร้างหลาย Pod แบบ 1 GPU แทน:

```text
Pod A: 1 GPU, workers @1-@2
Pod B: 1 GPU, workers @1-@2
Pod C: 1 GPU, workers @1-@2
```

## 2. สร้าง Database Secret

ห้ามใส่ MongoDB connection string ลงใน Docker image หรือ Template แบบ plain text

1. เปิด Runpod Console
2. ไปที่ **Secrets**
3. กด **Create Secret**
4. ตั้งชื่อ `vdohide_database_url`
5. ใส่ MongoDB connection string เป็น Secret Value
6. บันทึก Secret

เมื่อกำหนด Environment Variable ใน Template ให้ตั้ง:

```text
DATABASE_URL={{ RUNPOD_SECRET_vdohide_database_url }}
```

หรือกดไอคอนรูปกุญแจในหน้า Environment Variables แล้วเลือก Secret จาก UI

## 3. สร้าง Pod Template

1. ไปที่ **Templates**
2. กด **New Template**
3. ตั้งชื่อ เช่น `vdohide-transcode`
4. เลือก Template type เป็น Pod/ไม่ใช่ Serverless
5. ตั้ง Container Image เป็น:

```text
nvidia/cuda:12.8.1-runtime-ubuntu24.04
```

6. ตั้ง Container Disk เป็น `100 GB`
7. ไม่ต้องเพิ่ม Network Volume ในรอบแรก
8. เพิ่ม port `8886/http` เฉพาะกรณีต้องใช้หน้า Dashboard
9. เพิ่ม Environment Variables ตามตารางด้านล่าง

### Environment Variables

| Key | Value | หมายเหตุ |
|---|---|---|
| `DATABASE_URL` | `{{ RUNPOD_SECRET_vdohide_database_url }}` | จำเป็น |
| `WORKER_COUNT` | `1` | เริ่มหนึ่ง process ต่อ GPU แล้ว benchmark ก่อนเพิ่ม |
| `MEDIA_LAYOUT` | `separated` | ให้ตรงกับระบบปัจจุบัน |
| `S3_UPLOAD_CONCURRENCY` | `2` | เป็น concurrency ต่อไฟล์; adaptive mode อาจ upload หลายไฟล์พร้อมกัน |
| `DASHBOARD_PORT` | `8886` | worker `@1` เป็นคนเปิด port |
| `STORAGE_PATH` | `/opt/worker-transcode` | ใช้สำหรับรายงานและตรวจ disk |
| `TRANSCODE_PIPELINE_MODE` | `adaptive` | `adaptive`, `fanout` หรือ `sequential` |
| `TRANSCODE_FANOUT_MAX_MINUTES` | `30` | ความยาวสูงสุดที่ GPU fanout ทุก resolution พร้อมกัน |
| `TRANSCODE_UPLOAD_OVERLAP` | `true` | adaptive CPU encode ตัวถัดไประหว่าง upload ตัวก่อนหน้า |
| `TRANSCODE_MAX_PARALLEL_UPLOADS` | `2` | จำกัด uploads เบื้องหลังและทำ backpressure เมื่อ S3 ช้า |

ไม่จำเป็นต้องตั้ง `WORKER_ID` ใน Template เพราะ startup command จะสร้าง ID แยกให้
แต่ละ process โดยใช้ `RUNPOD_POD_ID`

## 4. ตั้ง Docker Start Command

`install.sh` ของ repository ใช้ systemd จึงไม่ควรเรียกตรงๆ ภายใน Runpod Pod
ให้ Template ดาวน์โหลด release และเปิด worker processes โดยตรงแทน

นำคำสั่งต่อไปนี้ใส่ในช่อง **Docker Start Command** ของ Template ถ้า UI แยกช่อง
Entrypoint ให้ปล่อย Entrypoint ว่าง และให้ Start Command เริ่มด้วย `bash -lc` ตามนี้:

```bash
bash -lc '
set -Eeuo pipefail

app_dir=/opt/worker-transcode
worker_count=${WORKER_COUNT:-1}

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  ca-certificates curl ffmpeg tini

install -d "$app_dir"
curl -fsSL \
  https://github.com/zergolf1994/worker-transcode/releases/latest/download/linux \
  -o "$app_dir/worker-transcode"
chmod +x "$app_dir/worker-transcode"

command -v nvidia-smi >/dev/null
nvidia-smi
ffmpeg -hide_banner -encoders 2>/dev/null | grep -q h264_nvenc
ffmpeg -hide_banner -loglevel error \
  -f lavfi -i color=c=black:s=256x256:d=1:r=25 \
  -pix_fmt yuv420p -c:v h264_nvenc \
  -frames:v 1 -f null -

worker_host=${RUNPOD_POD_ID:-$(hostname)}
worker_pids=()

shutdown_workers() {
  if ((${#worker_pids[@]} > 0)); then
    kill -TERM "${worker_pids[@]}" 2>/dev/null || true
    wait "${worker_pids[@]}" 2>/dev/null || true
  fi
}

trap shutdown_workers TERM INT EXIT

cd "$app_dir"
for worker_number in $(seq 1 "$worker_count"); do
  env WORKER_ID="transcode_runpod-${worker_host}@${worker_number}" \
    "$app_dir/worker-transcode" &
  worker_pids+=("$!")
done

set +e
wait -n "${worker_pids[@]}"
exit_status=$?
set -e

shutdown_workers
trap - TERM INT EXIT
exit "$exit_status"
'
```

คำสั่งนี้ทำสิ่งต่อไปนี้ทุกครั้งที่ Pod เริ่ม:

1. ติดตั้ง FFmpeg และเครื่องมือที่จำเป็น
2. ดาวน์โหลด Linux binary จาก GitHub Release ล่าสุด
3. ทดสอบ `h264_nvenc` จริงก่อนเปิด worker
4. เปิด worker ตามจำนวน `WORKER_COUNT`
5. ส่ง SIGTERM ให้ทุก worker เมื่อ Pod ถูก Stop เพื่อให้ worker คืนงานเข้าคิว

การติดตั้ง package ทุกครั้งเพิ่มเวลา startup เล็กน้อย แต่ทำให้เริ่มใช้งานได้โดยยัง
ไม่ต้องดูแล custom Docker image เมื่อการตั้งค่าคงที่แล้วจึงค่อยสร้าง image ที่ติดตั้ง
FFmpeg และ binary ไว้ล่วงหน้า

## 5. Deploy Pod

1. ไปที่ **Pods** แล้วกด **Deploy**
2. เลือก GPU ที่ต้องการ
3. ตั้ง **GPU Count = 1**
4. ตรวจจำนวน vCPU และ RAM ของ host
5. เลือก Template `vdohide-transcode`
6. ตรวจ Container Disk ว่าเป็น `100 GB`
7. กด Deploy

เกณฑ์เลือก host เบื้องต้น:

```text
2 workers ต้องการอย่างน้อยประมาณ 6-8 vCPU
3 workers ควรมีอย่างน้อยประมาณ 9-12 vCPU
4 workers ควรมีอย่างน้อยประมาณ 12-16 vCPU
```

CPU ที่ต้องใช้จริงขึ้นกับ codec, frame rate และ resolution ของ original

## 6. ตรวจหลัง Pod เริ่มทำงาน

ดู Container Logs จากหน้า Runpod ควรเห็นข้อความประมาณนี้:

```text
GPU Detected: NVIDIA (h264_nvenc)
Encoder: h264_nvenc
Starting Worker Transcode ... [Worker: transcode_runpod-...@1]
Starting Worker Transcode ... [Worker: transcode_runpod-...@2]
```

ถ้าเปิด terminal เข้า Pod ได้ ให้ตรวจ:

```bash
nvidia-smi
ffmpeg -hide_banner -encoders | grep nvenc
ffmpeg -hide_banner -hwaccels
ps aux | grep '[w]orker-transcode'
```

ควรพบ `h264_nvenc`, `cuda` และจำนวน process เท่ากับ `WORKER_COUNT`

## 7. ตั้งค่าใน MongoDB

ตรวจ document ชื่อ `transcode_config` ใน collection `settings`:

```json
{
  "enabled": true,
  "slotRate": 2,
  "gpuEnabled": true
}
```

- `enabled` เปิดการสร้างและรับงาน transcode
- `slotRate` เป็นขนาดคิวต่อ worker slot ไม่ใช่จำนวน GPU
- `gpuEnabled` อนุญาตให้ worker ใช้ `h264_nvenc`

เมื่อ Local GTX 1080 และ Runpod ทำงานพร้อมกัน จะเห็น worker IDs คนละชื่อใน
collection `workers` และแต่ละ process จะ claim งานได้ครั้งละหนึ่งงาน

## 8. เปิด Dashboard

ถ้ากำหนด `8886/http` ไว้ ให้เปิด Connect/HTTP Service ของ port 8886 จากหน้า Pod
หน้า Dashboard ของ worker `@1` จะแสดงงานของ sibling workers ใน Pod เดียวกันจาก
MongoDB

> Dashboard ปัจจุบันไม่มีระบบ login อย่าเปิด public URL ทิ้งไว้ถ้ามีข้อมูลที่ไม่ควร
> เปิดเผย ควรจำกัดการเข้าถึงหรือไม่ expose port นี้ใน production

## 9. ปรับจำนวน Worker เพื่อหาจุดคุ้มที่สุด

เริ่มจากค่าต่ำก่อน:

```text
RTX 3090: WORKER_COUNT=1; ทดลอง 2
RTX 4090: WORKER_COUNT=1; ทดลอง 2
RTX 5090: WORKER_COUNT=1; ทดลอง 2
RTX PRO 4500: WORKER_COUNT=1; ทดลอง 2
```

ระหว่างมีงานให้ตรวจ:

```bash
nvidia-smi dmon -s u
top
```

หรือ:

```bash
watch -n 1 'nvidia-smi --query-gpu=name,utilization.gpu,utilization.encoder,utilization.decoder,memory.used,temperature.gpu,power.draw --format=csv'
```

เพิ่ม `WORKER_COUNT` ครั้งละ 1 แล้ว Deploy/Restart ใหม่ โดยใช้เกณฑ์:

- CPU ควรต่ำกว่า 85% เป็นส่วนใหญ่
- NVENC ควรขึ้นประมาณ 70-95% ระหว่างช่วง encode
- เครื่องไม่ควร swap
- disk ไม่ควรเต็มเกิน 90%
- จำนวนงานที่เสร็จต่อชั่วโมงต้องเพิ่มขึ้นจริง
- error จาก S3 และ HTTP download ต้องไม่เพิ่มขึ้น

ถ้า CPU เต็มแต่ Encoder utilization ต่ำ แสดงว่า CPU scaling เป็นคอขวด การเพิ่ม
worker หรือเปลี่ยนไปใช้ GPU ที่ใหญ่ขึ้นจะไม่ช่วยมากนัก

## 10. วัดความคุ้มค่าเทียบกับ GTX 1080 Local

ใช้ original ไฟล์เดียวกัน ความยาวและ resolution เดียวกัน แล้ววัดอย่างน้อย 3 งาน:

```text
throughput = นาทีวิดีโอที่แปลงเสร็จ / นาทีเวลาจริง
cost/job   = ราคา Pod ต่อชั่วโมง × เวลางานเป็นวินาที / 3600
```

ควรเปรียบเทียบทั้ง:

- GTX 1080 Local, 2 workers
- RTX 3090 Runpod, 2 workers
- RTX 4090 Runpod, 2 workers
- RTX 4090 Runpod, 3 workers เมื่อ CPU เพียงพอ

GPU ที่เร็วที่สุดอาจไม่ใช่ GPU ที่มี `cost/job` ต่ำที่สุด

## 11. Storage และการ Stop Pod

โปรแกรมเก็บ original และ rendition ชั่วคราวใน directory `transcode` ข้าง binary
คู่มือนี้วาง binary ที่ `/opt/worker-transcode` จึงใช้ local container disk ที่:

```text
/opt/worker-transcode/transcode/<slug>
```

ขนาด disk ที่ควรมีโดยประมาณ:

```text
WORKER_COUNT × (ขนาด original ใหญ่สุด + ผลรวม outputs ทุก resolution + 20%)
```

ตัวอย่าง original ใหญ่สุด 20 GB และ 2 workers ควรมีพื้นที่ว่างอย่างน้อยประมาณ
50-60 GB; การตั้ง Container Disk 100 GB จึงเป็นจุดเริ่มต้นที่ปลอดภัยกว่า

- **Stop Pod** เมื่อจะกลับมาใช้เครื่องเดิมภายหลัง
- **Terminate Pod** เมื่อเลิกใช้และไม่ต้องการเสียค่า storage ต่อ
- ข้อมูลนอก Network Volume ควรถูกมองว่าเป็นข้อมูลชั่วคราว เพราะการแก้ Pod,
  recreate หรือ terminate อาจทำให้หายได้
- ถ้า Pod หยุดแบบ graceful worker จะ release งาน processing คืนเป็น pending
- ถ้า working files หาย งานจะดาวน์โหลด original และ encode ใหม่ใน retry รอบถัดไป

Network Volume ช่วยเก็บ retry cache ข้ามการ recreate แต่เพิ่มค่าใช้จ่ายและอาจช้ากว่า
local disk สำหรับไฟล์วิดีโอขนาดใหญ่ จึงควรวัดก่อนใช้กับ transcode path

## 12. Troubleshooting

### Pod เปิดแล้ว worker ใช้ CPU

ตรวจ:

```bash
nvidia-smi
ldconfig -p | grep -E 'libnvidia-encode|libnvcuvid'
ffmpeg -hide_banner -encoders | grep h264_nvenc
```

และดู startup log ว่าการทดสอบ NVENC ผ่านหรือไม่ หากใช้ RTX 5090 หรือ RTX PRO
Blackwell ให้ใช้ host driver และ CUDA-compatible image รุ่นใหม่พอ

### Worker process เปิดไม่ครบ

```bash
echo "$WORKER_COUNT"
ps aux | grep '[w]orker-transcode'
```

ตรวจว่าแต่ละ process มี `WORKER_ID` ไม่ซ้ำกัน ถ้า ID ซ้ำ instance lock จะปฏิเสธ
process ตัวหลัง

### งานไม่เข้า

ตรวจว่า:

- `vdohide-service` กำลังทำงานและสร้างคิว
- `transcode_config.enabled=true`
- `transcode_config.gpuEnabled=true`
- worker มี heartbeat ใหม่ใน collection `workers`
- worker record ไม่ถูก Admin disable
- MongoDB อนุญาต network จาก Runpod
- Runpod เข้าถึง source `originUrl` และ S3 ปลายทางได้

### Disk เต็ม

```bash
df -h /opt/worker-transcode
du -sh /opt/worker-transcode/transcode/* 2>/dev/null
```

worker จะหยุด claim งานใหม่เมื่อ disk usage ถึงประมาณ 90% แต่ควรขยาย disk หรือ
ลด `WORKER_COUNT` ก่อนถึงจุดนั้น

## 13. Adaptive pipeline

ค่า default คือ `TRANSCODE_PIPELINE_MODE=adaptive` และใช้กฎดังนี้:

| Encoder/ไฟล์ | Pipeline |
|---|---|
| CPU ทุกไฟล์ | sequential ทีละ resolution |
| GPU และความยาวไม่เกิน threshold | decode ครั้งเดียวแล้ว fanout ทุก resolution |
| GPU, ยาวเกิน threshold, สูงสุดไม่เกิน 480p | fanout 360p/480p พร้อมกัน |
| GPU, ยาวเกิน threshold, มี 720p ขึ้นไป | encode/upload 360p ก่อน แล้ว fanout ที่เหลือ |

ในกรณีสุดท้าย upload 360p จะทำพร้อมกับการ encode 480p/720p/1080p แต่ระบบจะ
ไม่เริ่ม publish คุณภาพสูงจนกว่า 360p จะอัปโหลดและสร้าง Media/Ingest สำเร็จ

ถ้า GPU fanout ล้มเหลว worker จะตรวจเก็บ output ที่สมบูรณ์ ลบเฉพาะไฟล์ที่ไม่สมบูรณ์
แล้ว fallback เป็น CPU sequential สำหรับ resolution ที่ขาด

เมื่อ adaptive เลือก CPU sequential และ `TRANSCODE_UPLOAD_OVERLAP=true` จะมี FFmpeg
encode เพียงหนึ่งตัวเหมือนเดิม แต่หลัง output ผ่าน validation แล้ว upload จะทำเบื้องหลัง
พร้อมกับ encode resolution ถัดไป จำนวน upload พร้อมกันถูกจำกัดด้วย
`TRANSCODE_MAX_PARALLEL_UPLOADS`; ถ้าคิว upload เต็ม encoder จะรอก่อนเริ่มตัวถัดไป

โหมด `sequential` แบบ explicit ยังคงพฤติกรรมเดิม `encode → upload → resolution ถัดไป`
เพื่อใช้ rollback โดยไม่เปิด upload overlap

สามารถ rollback โดยเปลี่ยนเป็น:

```env
TRANSCODE_PIPELINE_MODE=sequential
```

ขั้นต่อไปที่ยังทำเพิ่มได้คือใช้ `-hwaccel_output_format cuda` ร่วมกับ `scale_cuda`
หรือ `scale_npp` เพื่อลด CPU scaling และเพิ่มการกำหนด GPU index ก่อนใช้ Multi-GPU Pod

## เอกสารอ้างอิง

- [Runpod: Manage Pod templates](https://docs.runpod.io/pods/templates/manage-templates)
- [Runpod: Environment variables](https://docs.runpod.io/pods/templates/environment-variables)
- [Runpod: Manage secrets](https://docs.runpod.io/pods/templates/secrets)
- [Runpod GPU pricing](https://www.runpod.io/pricing)
- [NVIDIA FFmpeg GPU acceleration](https://developer.nvidia.com/ffmpeg)
- [NVIDIA Video Encode and Decode support matrix](https://developer.nvidia.com/video-encode-decode-support-matrix)
