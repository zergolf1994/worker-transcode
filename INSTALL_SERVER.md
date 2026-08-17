# ติดตั้ง Worker Transcode บน Hetzner

คู่มือนี้ใช้กับ Dedicated Server รุ่นที่นำมาใช้งานจริง:

- Intel Core i7-7700 (4 cores / 8 threads)
- NVIDIA GeForce GTX 1080 (Pascal, 2 NVENC engines)
- RAM 64 GB DDR4
- SSD SATA 512 GB จำนวน 2 ลูก
- Network 1 Gbit/s, traffic unlimited
- Location: Finland, HEL1
- Ubuntu Server 24.04 LTS
- Worker จำนวนเริ่มต้น: 2 instances

> คำเตือน: ขั้นตอน `installimage` จะล้างข้อมูลใน SSD ทั้งสองลูกทั้งหมด

## 1. เปิด Hetzner Rescue System

1. เข้า Hetzner Robot และเลือก server
2. เปิดหน้า **Rescue**
3. เลือก **Linux 64 bit** แล้วกด Activate
4. เก็บรหัสผ่าน Rescue ที่ระบบแสดง
5. สั่ง **Hardware Reset**
6. SSH เข้า server ด้วย IPv4 หลัก

```bash
ssh root@SERVER_IP
```

ตรวจว่าเห็น SSD และ GPU ครบ:

```bash
lsblk -o NAME,SIZE,MODEL,TYPE
lspci | grep -Ei 'vga|3d|nvidia'
```

ควรเห็น SSD ประมาณ 512 GB สองลูกเป็น `/dev/sda` และ `/dev/sdb` รวมถึง
`GeForce GTX 1080`

## 2. ติดตั้ง Ubuntu 24.04 ด้วย installimage

เรียกตัวติดตั้ง:

```bash
installimage
```

เลือกตามลำดับ:

```text
Ubuntu
Ubuntu 24.04 LTS (Noble) base
```

ในหน้า editor ให้ตรวจหรือแก้เฉพาะค่าหลักต่อไปนี้ ชื่ออุปกรณ์ต้องใช้ค่าที่
`installimage` ตรวจพบจริง:

```text
DRIVE1 /dev/sda
DRIVE2 /dev/sdb

SWRAID 1
SWRAIDLEVEL 1

HOSTNAME transcode-gpu-1
```

ใช้ RAID 1 เพราะพื้นที่ประมาณ 512 GB เพียงพอสำหรับ working files และยังบูตต่อได้
หาก SSD เสียหนึ่งลูก

ตั้ง partition ดังนี้:

```text
PART /boot ext4 1G
PART swap swap 8G
PART / ext4 all
```

ถ้า template มีบรรทัด EFI ให้เก็บไว้ก่อน `/boot`:

```text
PART /boot/efi esp 256M
```

ถ้า template ไม่มี `/boot/efi` แสดงว่าเครื่องบูตแบบ Legacy BIOS ไม่ต้องเพิ่มเอง
และให้เก็บค่า `IMAGE` ของ Ubuntu 24.04 ที่ตัวติดตั้งสร้างมาไว้ตามเดิม

กด `F10` เพื่อบันทึกและออก ยืนยันการล้างดิสก์ แล้วรอจนติดตั้งสำเร็จ จากนั้น:

```bash
reboot
```

รหัสผ่าน root หลังติดตั้งคือรหัส Rescue เดิม

## 3. ตรวจ RAID และสุขภาพ SSD

SSH กลับเข้า server แล้วตรวจ RAID:

```bash
cat /proc/mdstat
lsblk -f
```

ถ้า RAID กำลัง sync ให้ดูความคืบหน้าได้ด้วย:

```bash
watch -n 5 cat /proc/mdstat
```

ติดตั้งเครื่องมือตรวจ SSD และเปิด TRIM:

```bash
apt update
apt install -y smartmontools sysstat htop iotop
systemctl enable --now fstrim.timer

smartctl -a /dev/sda
smartctl -a /dev/sdb
```

ควรตรวจว่าไม่มี SMART error, reallocated sector หรือ uncorrectable error ก่อนเริ่มงาน

## 4. อัปเดต Ubuntu

```bash
apt update
DEBIAN_FRONTEND=noninteractive apt full-upgrade -y
apt install -y \
    ubuntu-drivers-common \
    linux-headers-$(uname -r) \
    curl \
    ca-certificates \
    ffmpeg
reboot
```

## 5. ติดตั้ง NVIDIA driver

GTX 1080 ต้องใช้ NVIDIA proprietary driver อย่าติดตั้งไฟล์ `.run` จากเว็บไซต์ NVIDIA
โดยตรง ให้ใช้ package ของ Ubuntu เพื่อให้ kernel update ได้ปลอดภัย

ตรวจ driver ที่ Ubuntu แนะนำ:

```bash
ubuntu-drivers list --gpgpu
```

ติดตั้ง server/compute driver ที่เหมาะกับ GPU อัตโนมัติ:

```bash
ubuntu-drivers install --gpgpu
reboot
```

หลัง reboot ตรวจว่า driver ทำงาน:

```bash
cat /proc/driver/nvidia/version
```

ติดตั้ง `nvidia-smi` และ NVENC/NVDEC user-space runtime ให้ตรงกับ branch ของ
driver เช่น driver `580.173.02` ต้องใช้แพ็กเกจ branch `580` โดยอ่าน branch จาก
kernel module ก่อน เพราะเครื่องอาจยังไม่มีคำสั่ง `nvidia-smi`:

```bash
DRIVER_BRANCH=$(awk '/NVRM version:/ {
    for (i = 1; i <= NF; i++) {
        if ($i ~ /^[0-9]+\./) {
            split($i, version, ".")
            print version[1]
            exit
        }
    }
}' /proc/driver/nvidia/version)

test -n "$DRIVER_BRANCH" || {
    echo "Cannot detect NVIDIA driver branch"
    exit 1
}

apt update

if apt-cache show "nvidia-utils-${DRIVER_BRANCH}" >/dev/null 2>&1; then
    NVIDIA_UTILS_PACKAGE="nvidia-utils-${DRIVER_BRANCH}"
elif apt-cache show "nvidia-utils-${DRIVER_BRANCH}-server" >/dev/null 2>&1; then
    NVIDIA_UTILS_PACKAGE="nvidia-utils-${DRIVER_BRANCH}-server"
else
    echo "No nvidia-utils package found for driver branch ${DRIVER_BRANCH}"
    exit 1
fi

apt install -y \
    "$NVIDIA_UTILS_PACKAGE" \
    "libnvidia-encode-${DRIVER_BRANCH}" \
    "libnvidia-decode-${DRIVER_BRANCH}"
ldconfig
```

ตรวจว่าคำสั่งถูกติดตั้งและมองเห็น GPU:

```bash
command -v nvidia-smi
nvidia-smi
```

ถ้า `command -v nvidia-smi` ไม่แสดง path ห้ามข้ามขั้นตอนนี้ เพราะ FFmpeg อาจใช้
NVENC ได้ แต่หน้า Transcode Monitor จะไม่สามารถอ่าน GPU, VRAM, NVENC และ NVDEC
และจะแสดง `N/A`

ตรวจว่า dynamic linker มองเห็น library ที่ FFmpeg ต้องใช้:

```bash
ldconfig -p | grep -E 'libnvidia-encode|libnvcuvid'
```

ต้องพบ `libnvidia-encode.so.1` และ `libnvcuvid.so.1` การที่คำสั่ง
`ffmpeg -encoders` แสดง `h264_nvenc` เพียงอย่างเดียวยังไม่ยืนยันว่า runtime
พร้อม เพราะ encoder อาจถูก compile มาแล้วแต่ library ของ driver ยังไม่มี

เปิด persistence mode เพื่อลดเวลาเริ่มต้น NVENC แต่ละงาน:

```bash
nvidia-smi -pm 1
systemctl enable --now nvidia-persistenced 2>/dev/null || true
```

## 6. ตรวจ FFmpeg และ NVENC

ตรวจ encoder, decoder และ hardware acceleration:

```bash
ffmpeg -hide_banner -encoders | grep nvenc
ffmpeg -hide_banner -decoders | grep -E 'cuvid|nvdec'
ffmpeg -hide_banner -hwaccels
```

อย่างน้อยต้องเห็น:

```text
h264_nvenc
cuda
```

ทดสอบ H.264 NVENC จริง:

```bash
ffmpeg -hide_banner -loglevel info \
    -f lavfi \
    -i color=c=black:s=1920x1080:d=5:r=30 \
    -pix_fmt yuv420p \
    -c:v h264_nvenc \
    -preset p5 \
    -tune hq \
    -f null -
```

คำสั่งต้องจบโดยไม่มี `No capable devices found`, `Cannot load libcuda` หรือ
`Cannot load libnvidia-encode.so.1`

## 7. ตั้งค่า transcode ในระบบ

ตรวจ setting `transcode_config` ใน collection `settings`:

```json
{
  "enabled": true,
  "slotRate": 2,
  "gpuEnabled": true
}
```

- `enabled`: เปิดการสร้างคิว transcode
- `slotRate`: จำนวนงานในคิวต่อ worker slot
- `gpuEnabled`: อนุญาตให้ worker auto-detect `h264_nvenc`

## 8. ติดตั้ง worker-transcode

`install.sh` จะไม่ติดตั้งหรือเปลี่ยน NVIDIA kernel driver แต่เมื่อตรวจพบ driver
ที่ทำงานอยู่ มันจะติดตั้ง `libnvidia-encode-*`/`libnvidia-decode-*` branch เดียวกัน
หากยังไม่มี และรันทดสอบ `h264_nvenc` ก่อนเริ่ม service

GTX 1080 มี NVENC สอง engines จึงเริ่มด้วย worker 2 instances:

```bash
curl -fsSL \
    https://raw.githubusercontent.com/zergolf1994/worker-transcode/main/install.sh \
    | sudo -E bash -s -- \
        --database-url 'MONGODB_CONNECTION_STRING' \
        --count 2
```

ใช้ single quote ครอบ MongoDB URL เพื่อป้องกัน `$`, `&` หรืออักขระพิเศษถูก shell
ตีความ และอย่าเขียน connection string จริงลง Git

ตรวจ service:

```bash
systemctl status worker-transcode@1 --no-pager
systemctl status worker-transcode@2 --no-pager
journalctl -u 'worker-transcode@*' -n 100 --no-pager
```

ดู log แบบ realtime:

```bash
journalctl -u 'worker-transcode@*' -f
```

เมื่อรับงาน log ควรมีข้อความลักษณะนี้:

```text
GPU Detected: NVIDIA (h264_nvenc)
Encoder: h264_nvenc
```

## 9. ตรวจโหลดระหว่าง transcode

หน้า realtime monitor เปิดที่ port `8886` โดยมีเพียง `worker-transcode@1`
เป็นตัวเปิดเว็บ ถึงแม้ติดตั้ง 2 instances หน้านี้ยังแสดง job ของทั้งสองตัวจาก
MongoDB พร้อม GPU/NVENC, CPU, memory, disk usage และ disk read/write ต่อวินาที

อนุญาตให้เฉพาะ IP ของผู้ดูแลเข้าถึง (หน้า monitor ไม่มีระบบ login):

```bash
sudo ufw allow from YOUR_ADMIN_IP to any port 8886 proto tcp
```

จากนั้นเปิด `http://SERVER_IP:8886`

เปิด terminal แยกเพื่อตรวจ GPU:

```bash
watch -n 1 "$(command -v nvidia-smi)"
```

ตรวจ CPU, disk และ network:

```bash
htop
iostat -xz 1
```

สำหรับ i7-7700 แนะนำสูงสุดเริ่มต้น 2 workers เพราะ scaling และ AAC ยังใช้ CPU
ถ้า CPU อยู่ที่ 90-100% ต่อเนื่อง, load average สูง หรือเครื่องเริ่ม swap ให้หยุด worker
ตัวที่สอง:

```bash
systemctl disable --now worker-transcode@2
```

ถ้าโหลดปกติและต้องการเปิดกลับ:

```bash
systemctl enable --now worker-transcode@2
```

## 10. คำสั่งดูแลระบบ

Restart workers ทั้งหมด:

```bash
for i in $(seq 1 2); do systemctl restart worker-transcode@$i; done
```

Stop workers ทั้งหมด:

```bash
for i in $(seq 1 2); do systemctl stop worker-transcode@$i; done
```

ตรวจพื้นที่และ working files:

```bash
df -h
du -sh /opt/worker-transcode/transcode 2>/dev/null || true
```

ตรวจอุณหภูมิและการใช้ NVENC:

```bash
nvidia-smi --query-gpu=name,temperature.gpu,power.draw,utilization.gpu,utilization.encoder,utilization.decoder,memory.used --format=csv
```

## 11. Troubleshooting

### Worker ใช้ CPU แทน GPU

```bash
nvidia-smi
ffmpeg -hide_banner -encoders | grep h264_nvenc
journalctl -u worker-transcode@1 -n 100 --no-pager
```

ถ้า `nvidia-smi` ใช้ไม่ได้ ให้ตรวจ kernel module:

```bash
lsmod | grep nvidia
journalctl -k | grep -iE 'nvidia|nouveau'
```

ถ้า driver โหลดอยู่ แต่พบ `nvidia-smi: command not found` ให้ติดตั้ง utilities
branch เดียวกับ driver ตามขั้นตอนที่ 5 ตัวอย่างสำหรับ branch 580:

```bash
apt update
apt install -y nvidia-utils-580 || apt install -y nvidia-utils-580-server
command -v nvidia-smi
nvidia-smi
systemctl restart 'worker-transcode@*'
```

จากนั้นลองติดตั้ง driver ใหม่และ reboot:

```bash
ubuntu-drivers install --gpgpu
reboot
```

### NVENC ทำงานแต่ช้า

ตรวจว่า CPU scaling หรือ disk/network เป็นคอขวด:

```bash
htop
iostat -xz 1
watch -n 1 nvidia-smi
```

ถ้า GPU encoder utilization ต่ำแต่ CPU เต็ม ให้ลดเหลือ 1 worker

### งานไม่เข้า queue

ตรวจว่า:

- `vdohide-service` ทำงานอยู่
- มี worker heartbeat ชนิด `transcode` และสถานะไม่ disabled
- `transcode_config.enabled=true`
- `transcode_config.gpuEnabled=true`
- ไฟล์มีสถานะ `ready` และมี Media resolution `original`
- Worker เข้าถึง MongoDB และ `originUrl` ของ storage ได้

## เอกสารอ้างอิง

- [Hetzner installimage](https://docs.hetzner.com/robot/dedicated-server/operating-systems/installimage/)
- [Hetzner standard images](https://docs.hetzner.com/robot/dedicated-server/operating-systems/standard-images/)
- [Ubuntu NVIDIA driver installation](https://ubuntu.com/server/docs/nvidia-drivers-installation/)
- [NVIDIA video encode/decode support matrix](https://developer.nvidia.com/video-encode-decode-support-matrix)
- [NVIDIA FFmpeg GPU acceleration](https://docs.nvidia.com/video-technologies/video-codec-sdk/13.1/ffmpeg-with-nvidia-gpu/index.html)
