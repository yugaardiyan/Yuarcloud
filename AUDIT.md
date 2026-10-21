# Audit Yuarcloud

Tanggal: 22 Oktober 2026
Cakupan: backend Go (`cmd/`, `internal/`), frontend React (`web/src/`), konfigurasi, database, dan deployment.
Metode: empat audit baca-saja paralel (auth/keamanan, operasi file/storage, frontend, config/DB/deploy), setiap temuan diverifikasi ulang dengan membaca kode, lalu diperbaiki dan diuji pada instance terpisah (port 3104, database sendiri) atau dengan tes Go. Aplikasi asli di port 3103 tidak dipakai untuk pengujian yang mengubah data.

Baseline sebelum perubahan: `go vet ./...`, `go test ./...`, `go test -race ./internal/storage/... ./internal/handler/...`, `gofmt -l .`, dan `npm run lint` semuanya lolos. Artinya bug di bawah ini laten: tidak tertangkap tes yang ada.

Kode: ~13.100 baris Go (`internal/`), ~765 baris `cmd/`, ~14.000 baris frontend.

---

## Ringkasan

| Tingkat | Jumlah | Diperbaiki | Bukti empiris |
|---|---:|---:|---|
| Kritis | 13 | 13 | 9 direproduksi langsung, 4 diverifikasi lewat kode/header |
| Tinggi | 15 | 15 | 6 direproduksi, sisanya verifikasi kode + build |
| Sedang | 6 | 1 | 1 diperbaiki, 5 didokumentasikan (butuh keputusan/ubah API) |
| Rendah | 4 | 2 | 2 diperbaiki |

Tidak ada perubahan tampilan: semua perbaikan frontend menyentuh perilaku (state, URL, error handling), bukan markup atau CSS. Satu-satunya perubahan visual yang mungkin terasa adalah tombol simpan konfigurasi chunked tidak lagi menulis nilai default saat data belum termuat (tidak ada elemen baru).

---

## Kritis

### K1. Upload lewat share link bisa menghapus file pemilik (data loss)
- Lokasi: `internal/handler/share.go` → `ShareUpload`.
- Fakta: nama file dicek dengan `safeFilename`, tetapi tidak pernah dibandingkan dengan isi folder. `storage.Write` memakai `os.Create` (memotong file yang ada), lalu `db.Create` gagal karena indeks unik `path`, dan blok error memanggil `storage.Remove` — menghapus byte asli sementara baris DB lama tetap ada.
- Reproduksi (instance uji, sebelum perbaikan): folder `Uji-Tabrakan` berisi `laporan.txt` 10 byte; share write token `qX6cCR`; upload `laporan.txt` lain lewat `/api/shares/qX6cCR/upload` → HTTP 500, isi folder di disk kosong, baris `Uji-Tabrakan/laporan.txt` masih ada.
- Perbaikan: cek baris DB dan keberadaan file di disk sebelum menulis (409), dan saat insert gagal hanya hapus byte bila tidak ada baris hidup untuk path itu.
- Verifikasi: skenario sama sekarang → HTTP 409, file tetap 10 byte dan isinya `ASLI-12345`.

### K2. MIME dari klien → XSS same-origin lewat pratinjau
- Lokasi: `internal/handler/preview.go` → `serveInline`; `web/src/components/PreviewModal.jsx` (iframe PDF); `web/src/utils/fileType.js` (`kindOf` memilih PDF dari ekstensi).
- Fakta: tipe MIME disimpan apa adanya dari upload. File bernama `.pdf` dengan `Content-Type: text/html` dirender sebagai dokumen di origin aplikasi, sehingga skrip dapat membaca `localStorage` (token sesi).
- Perbaikan: respons pratinjau untuk tipe aktif (`text/html`, `application/xhtml+xml`, `image/svg+xml`, XML, JavaScript) diberi `Content-Security-Policy: sandbox; default-src 'none'; ...` tanpa `allow-scripts`. HTML/SVG tetap tampil, skrip tidak berjalan.
- Verifikasi: upload `evil.html` (`text/html`) lalu preview dengan token → header `Content-Security-Policy: sandbox; default-src 'none'; img-src data: blob:; style-src 'unsafe-inline'; media-src data: blob:` dan `X-Content-Type-Options: nosniff`.

### K3. Dua `complete` bersamaan menghapus file pemenang
- Lokasi: `internal/handler/file_chunked.go` → `CompleteChunkedUpload`.
- Fakta: tidak ada klaim sesi. Dua request lolos cek nama, merakit ke tujuan yang sama, lalu insert kedua gagal pada indeks unik dan blok pembersihannya menghapus file yang baru didaftarkan pemenang.
- Perbaikan: `UPDATE ... SET status='completing' WHERE id=? AND status='uploading'` sebagai klaim; yang kalah mendapat 409 sebelum menyentuh byte. Status dikembalikan ke `uploading` pada kegagalan yang bisa dicoba ulang.
- Verifikasi: complete pertama 201, complete kedua 409 (`this upload is already being finalised`); upload chunked normal 11 byte tetap utuh dan isinya benar.

### K4. Nama file sementara chunk bisa menabrak file pengguna
- Lokasi: `internal/storage/local_chunked.go` → `AssembleChunks`.
- Fakta: bagian sementara bernama `<tujuan>.yuarcloud-part`. `safeFilename` hanya menolak nama yang *diawali* `.yuarcloud`, jadi `report.pdf.yuarcloud-part` adalah nama sah. Upload file itu lalu chunk-upload `report.pdf` akan memotong dan mengganti file pertama.
- Perbaikan: `os.CreateTemp(dir, ".yuarcloud-part-*")` (nama acak, `O_EXCL`), plus penghapusan otomatis pada semua jalur gagal.
- Verifikasi: upload chunked normal berhasil; tidak ada lagi nama sementara yang bisa ditebak.

### K5. Image Docker tidak bisa membuka SQLite
- Lokasi: `Dockerfile` (build `CGO_ENABLED=0`), `go.mod` (`gorm.io/driver/sqlite` → `mattn/go-sqlite3`).
- Fakta: dengan cgo mati, driver membangun stub dan `gorm.Open` gagal saat start.
- Reproduksi lokal: `CGO_ENABLED=0 go build -o /tmp/yc-nocgo ./cmd/server` lalu dijalankan →
  `Failed to initialize database: open database: Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work. This is a stub`.
- Perbaikan: `CGO_ENABLED=1` pada build image. Runtime alpine sudah membawa musl.
- Batasan: tidak ada container runtime di mesin ini, jadi image belum dijalankan; yang dibuktikan adalah penyebab kegagalannya.

### K6. Bind mount ZimaOS menutupi config
- Lokasi: `Dockerfile`, `docker-entrypoint.sh`, `docker-compose.zimaos.yml`.
- Fakta: `config.yaml` hanya ada di layer image `/config`. Compose ZimaOS me-mount direktori host ke `/config`, sehingga layer itu tertutup dan server start tanpa config (gagal pada `jwt_secret`).
- Perbaikan: salinan kedua di `/app/config.docker.yaml`; entrypoint menyalinnya ke `/config/config.yaml` bila belum ada, lalu `chown` file itu.
- Batasan: belum dijalankan di container (tidak ada runtime).

### K7. Secret JWT placeholder diterima
- Lokasi: `cmd/server/main.go` (cek panjang saja), `docker-compose.zimaos.yml`, `.env.example`.
- Fakta: `PASTE-YOUR-OWN-64-CHARACTER-RANDOM-STRING-HERE` (49 karakter) dan `generate-with-openssl-rand-base64-48` (38 karakter) lolos cek `len >= 32`. Siapa pun yang membaca repo bisa memalsukan token admin pada deployment yang menyalin contoh itu.
- Perbaikan: `placeholderSecret()` menolak penanda seperti `paste-your-own`, `generate-with-openssl`, `change-me`, `placeholder`, `random-string`, dan sejenisnya; pesan error menyebut placeholder secara eksplisit.

### K8. Reset password oleh admin selalu gagal; `password_hash` bisa disuntikkan
- Lokasi: `internal/handler/admin_users.go` → `UpdateUser`.
- Fakta: allowlist memuat `password_hash` tetapi tidak `password`, sehingga cabang validasi+hash untuk `password` tidak pernah tercapai (400 `field cannot be updated here: password`), dan hash mentah bisa ditulis tanpa aturan panjang.
- Perbaikan: allowlist `password` (divalidasi dan di-hash), hapus `password_hash`.
- Verifikasi: `PATCH /api/admin/users/:id {"password":"rahasia-baru-123"}` → 200, login dengan password baru → 200; `{"password_hash":"x"}` → 400.

### K9. Demosi admin tidak mencabut akses
- Lokasi: `internal/admin/service.go` → `UpdateUserRole`; `internal/middleware/scope.go`.
- Fakta: rute khusus role hanya mengubah kolom `role`, tidak menaikkan `token_version`, sehingga token admin lama tetap sah sampai 24 jam.
- Perbaikan: `token_version = token_version + 1` pada perubahan role; middleware scope juga mengambil role dari baris DB, bukan dari klaim token.
- Verifikasi: `token_version` user 0 → 1 setelah demosi.

### K10. Admin terakhir bisa didemosi
- Lokasi: `internal/handler/admin_users.go` → `UpdateUserRole`.
- Fakta: rute PATCH umum punya penjaga "admin terakhir", rute role tidak. Demosi admin terakhir menutup satu-satunya jalan masuk panel (seed tidak membuat admin baru selama masih ada baris admin).
- Perbaikan: penjaga yang sama dipakai di rute role.
- Verifikasi: dengan dua admin, demosi biasa 200; percobaan demosi admin terakhir → 400 `this is the last admin account; promote another user first`.

### K11. Tab "Share Links" di admin bisa memblokir seluruh halaman
- Lokasi: `web/src/pages/Admin.jsx` → `SharesTab`.
- Fakta: cabang error merender `onClick={loadShares}`, padahal `loadShares` tidak ada di scope komponen itu (propnya `onReload`). Saat daftar share gagal dimuat, klik "Try again" melempar `ReferenceError` dan React mengosongkan tree.
- Perbaikan: memakai `onReload`.

### K12. Unduh ZIP folder selalu 401
- Lokasi: `web/src/context/FileContext.jsx` → `downloadItem`.
- Fakta: folder diserahkan ke `<a href>` tanpa header `Authorization`; browser meminta tanpa token. File biasa memakai `fetch` + header, folder tidak.
- Perbaikan: kedua cabang memakai `fetch` dengan header, lalu blob URL; objek URL dicabut setelah dipakai.

### K13. Init chunked menerima ukuran yang mustahil
- Lokasi: `internal/handler/file_chunked.go` → `InitChunkedUpload`.
- Fakta: `total_chunks` tidak pernah dibandingkan dengan `total_size`/`chunk_size`. `total_size: 1, total_chunks: 1000` membuka 1000 chunk berukuran maksimum sementara semua pemeriksaan kuota melihat 1 byte.
- Perbaikan: tolak ukuran nol/negatif dan wajibkan `total_chunks == ceil(total_size / chunk_size)`; sesi di memori dibersihkan bila insert DB gagal.
- Verifikasi: `{"total_size":1,"chunk_size":8388608,"total_chunks":1000}` → 400 `total_chunks does not match total_size and chunk_size`.

---

## Tinggi

### T1. Rute pratinjau melewati wajib-ganti-password
- Lokasi: `cmd/server/main.go` (grup `preview`).
- Fakta: akun yang masih memakai password default diblokir di semua rute kecuali change-password, tetapi grup preview tidak memasang middleware rotasi — akun itu masih bisa membaca semua file yang bisa dipratinjau.
- Perbaikan: `PasswordRotationRequiredMiddleware` dipasang di grup preview.

### T2. Ganti password menerima password yang sama
- Lokasi: `internal/handler/auth.go` → `ChangePassword`.
- Fakta: `must_rotate_pass` dibersihkan setelah perubahan apa pun, termasuk bila password baru sama dengan yang lama — jadi password default yang dipublikasikan bisa "diganti" dengan dirinya sendiri.
- Perbaikan: tolak bila password baru cocok dengan hash yang sekarang.

### T3. Share milik akun/berkas yang sudah dihapus tetap hidup
- Lokasi: `internal/share/service.go` → `ValidateShare`.
- Fakta: penjaga memakai `if share.User.ID != uuid.Nil && !share.User.IsActive`, sehingga ID nol (hasil preload untuk baris yang soft-deleted) justru melewati pemeriksaan. Sama untuk berkas.
- Perbaikan: ID nol sekarang ditolak.

### T4. Upload sederhana yang gagal tertahan di status "uploading"
- Lokasi: `web/src/utils/upload.js` → `uploadFilesSequentially`.
- Fakta: jalur chunked melaporkan error lewat `onError`, jalur sederhana hanya mendorong ke `failures`. Baris di tray tetap "uploading" dengan progres beku.
- Perbaikan: panggil `onUpdate` dengan `status: 'error'` dan pesan.

### T5. Folder gagal dibuat → isi folder diratakan ke induk
- Lokasi: `web/src/context/FileContext.jsx` → `uploadFiles`.
- Fakta: kegagalan membuat folder diikuti `dirId.set(dirPath, null)`, lalu `parentIdFor` memakai `?? baseParentId` — `null` dianggap "taruh di folder saat ini". Komentar kode mengatakan "stop the tree", perilakunya justru meratakan.
- Perbaikan: set `failedDirs`, dan file di bawah direktori yang gagal (atau turunannya) dilewati, bukan dipindahkan diam-diam.

### T6. Kegagalan sementara `/auth/me` menghapus sesi
- Lokasi: `web/src/context/AuthContext.jsx` → `fetchUser`.
- Fakta: semua error (timeout, jaringan putus, 5xx) menghapus `access_token` dan `refresh_token`, padahal kredensialnya masih sah.
- Perbaikan: hanya 401/403 yang menghapus token.

### T7. `DELETE /shares/:id` dianggap endpoint publik
- Lokasi: `web/src/utils/api.js` (interceptor 401).
- Fakta: deteksi "public share" hanya melihat awalan `/shares/`, sehingga 401 pada hapus share milik sendiri tidak memicu refresh token dan muncul sebagai kegagalan hapus.
- Perbaikan: hanya GET/POST pada `/shares/` yang dianggap publik.

### T8. Halaman 2 daftar user tidak pernah diminta
- Lokasi: `web/src/pages/Admin.jsx` → `loadUsers`.
- Fakta: `getUsers(params.toString())` mengirim string; axios mengharapkan objek, sehingga parameter halaman hilang dan halaman 1 diminta berulang.
- Perbaikan: kirim objek `{ page, page_size }`.

### T9. Simpan konfigurasi chunked menimpa nilai server dengan default
- Lokasi: `web/src/pages/Admin.jsx` → `ChunkedConfigTab`.
- Fakta: form mulai dengan `10 MB / 10 GB / 1h`. Bila tombol Save ditekan sebelum fetch selesai, nilai default itu yang dikirim.
- Perbaikan: `handleSave` keluar bila `config` belum ada.

### T10. Menu "Open" mengunduh, bukan membuka pratinjau
- Lokasi: `web/src/components/ContextMenuProvider.jsx`.
- Fakta: untuk file, aksi `open` memanggil `downloadItem`, sedangkan Enter dan klik ganda membuka pratinjau. Kata yang sama melakukan dua hal berbeda.
- Perbaikan: dispatch `yuar:preview`; `Drive.jsx` mendengarkan dan memanggil `openPreview`.
- Verifikasi browser: klik kanan `chunked-uji.bin` → Open → modal `preview-overlay` muncul, tidak ada unduhan.

### T11. Ctrl/Cmd-V di Trash/Search menempel ke root
- Lokasi: `web/src/components/ContextMenuProvider.jsx`.
- Fakta: destinasi dihitung `view.scope === 'drive' ? ... : null`, dan `null` berarti My Cloud. Menempel saat melihat Trash/Search menaruh file di tempat yang tidak dilihat pengguna.
- Perbaikan: abaikan pintasan bila scope bukan drive.

### T12. `driveHref` tidak melakukan encoding
- Lokasi: `web/src/context/FileContext.jsx` → `driveHref`.
- Fakta: nama folder berisi `#` mengubah sisa path menjadi fragmen; `%` bisa membuat segmen tidak bisa didekode. Parser sudah mendekode per segmen, pembangun URL belum melakukan encode.
- Perbaikan: `encodeURIComponent` per segmen.

### T13. Logout meninggalkan state akun sebelumnya
- Lokasi: `web/src/context/AuthContext.jsx` → `logout`.
- Fakta: `FileProvider` hidup lebih lama dari `setUser(null)`, jadi daftar file, seleksi, clipboard, dan tray akun lama masih tampil sampai navigasi berikutnya.
- Perbaikan: logout melakukan `window.location.assign('/login')` — reset penuh yang tidak bisa melewatkan satu state pun.

### T14. Batas ukuran upload dibekukan saat start
- Lokasi: `cmd/server/main.go` (registrasi rute), `internal/middleware/bodylimit.go`.
- Fakta: `BodyLimitMiddleware(store.GetMaxFileSize()+multipartOverhead)` dievaluasi sekali saat start, jadi perubahan dari panel admin baru berlaku setelah restart. Selain itu `max_file_size: 0` (tanpa batas) menjadi 1 MiB karena ditambah overhead.
- Perbaikan: `BodyLimitFunc` membaca limit per request; nilai 0 diteruskan sebagai "tanpa batas"; rute chunk memakai batas chunk, bukan batas file.

### T15. Rate limiter di belakang reverse proxy
- Lokasi: `internal/middleware/auth.go` → `RateLimitMiddleware`.
- Fakta: kunci memakai `RemoteIP()` selalu, sehingga di belakang nginx/Cloudflare semua pengguna berbagi satu bucket (satu klien bisa membatasi semua orang). Komentar di `main.go` mengklaim `SetTrustedProxies` mengembalikan atribusi per klien, padahal `RemoteIP` tidak terpengaruh.
- Perbaikan: `ClientIP()` hanya dipakai bila `server.trusted_proxies` diisi; selain itu tetap `RemoteIP()` agar header tidak bisa dipalsukan.

---

## Sedang / rendah

| # | Temuan | Status |
|---|---|---|
| S1 | Password share dan token pratinjau dikirim di query string, sehingga bisa tercatat di access log/referrer. | Belum diperbaiki — butuh perubahan kontrak API (header/`POST`), bukan sekadar bug. |
| S2 | `server.write_timeout: 30s` pada file server berisiko memotong unduhan/ZIP yang sangat besar di koneksi lambat. Uji 10 MB pada loopback tetap utuh (kernel menyangga), jadi belum terbukti. | Belum diubah — perlu keputusan; saran: `0` atau beberapa jam. |
| S3 | Akuntansi chunk duplikat: kirim chunk 0 byte untuk semua indeks, lalu kirim ulang indeks yang sama dengan ukuran besar. `ReceivedBytes` tidak bertambah karena cabang duplikat kembali lebih dulu. Dibatasi oleh validasi init + batas body chunk. | Sebagian termitigasi; akuntansi penuh belum diperbaiki. |
| S4 | Tidak ada migrasi skema (hanya GORM AutoMigrate); jalur PostgreSQL diiklankan tetapi tidak diuji. | Belum diperbaiki — pekerjaan terpisah. |
| S5 | `go test ./...` ikut menjelajahi `web/node_modules` (paket Go dari `flatted`), menambah noise. | Belum diperbaiki (higiene). |
| S6 | Image `ghcr.io` yang terbit tertinggal dari source; log GitHub Actions tidak bisa dibaca tanpa autentikasi. | Tidak bisa diverifikasi dari sini. |
| S7 | Panjang minimum password 8 karakter, sedangkan akun seed memakai `admin` (5). Reset admin tidak bisa mengembalikan nilai seed. | Dibiarkan; tidak berbahaya, tetapi tidak konsisten. |
| R1 | `expiry_days: -1` berarti "tanpa kedaluwarsa" dan UI memang menawarkannya — ini fitur, bukan bug, meski melewati `max_expiry_days`. | Sengaja; didokumentasikan. |
| R2 | `.dockerignore` tidak ada, sehingga konteks build mengirim `node_modules`, `dist`, DB, dan log. | Diperbaiki. |

---

## Optimasi (bukan bug)

1. **Indeks DB** sudah memadai untuk query panas: `files(path)` unik parsial, `files(user_id)`, `files(parent_id)`, `share_links(token)`, `audit_logs(created_at/action/user_id)`. Tidak ada N+1 yang menonjol di listing karena anak folder diambil per level.
2. **Bundle frontend** sudah dipisah: Admin/ShareView lazy, `mammoth` (299 KB) dan `xlsx` (492 KB) hanya dimuat saat pratinjau dokumen/spreadsheet.
3. **ZIP folder** dialirkan (`archive/zip` + `io.Copy`) ke respons, bukan disusun di memori. Aman untuk folder besar selain risiko S2.
4. **Refresh token** sudah dideduplikasi satu penerbangan (`refreshInFlight`), jadi beberapa 401 bersamaan tidak memicu beberapa rotasi.
5. **Chunked upload** menulis chunk lalu `fsync` sebelum mencatatnya — tahan crash, tetapi biaya I/O per chunk. Untuk filesystem modern, `fsync` per chunk bisa dipertimbangkan menjadi per beberapa chunk bila throughput jadi masalah.
6. **Rate limiter** menyimpan bucket di memori per proses; jika nanti jalan multi-instance, perlu store bersama.
7. **`internal/auth/preview_token_test.go`** bagus; menambah tes regresi untuk collision share/chunk akan mencegah K1/K3/K4 kembali.

---

## Verifikasi yang dijalankan

| Uji | Hasil |
|---|---|
| `go build ./...` | lolos |
| `go vet ./...` | lolos |
| `go test ./...` | lolos |
| `go test -race ./internal/storage/... ./internal/handler/...` | lolos |
| `gofmt -l .` (di luar `node_modules`) | bersih |
| `npm run lint` + `npm run build` | lolos |
| Share upload nama sama (sebelum) | 500, byte asli hilang, baris DB tertinggal |
| Share upload nama sama (sesudah) | 409, byte asli utuh |
| `PATCH` password admin | 200, login password baru 200 |
| `PATCH` `password_hash` | 400 |
| Demosi admin terakhir | 400 |
| Perubahan role | `token_version` 0 → 1 |
| Init chunk mustahil | 400 |
| Complete chunked dua kali | 201 lalu 409 |
| Upload chunked normal 11 byte | isi `ABCDEFGHIJ` benar |
| Header preview HTML | CSP `sandbox`, `nosniff` |
| Admin UI | halaman render, tab Chunked Upload memuat 10 MB/10 GB, tanpa error konsol |
| Menu Open | modal pratinjau terbuka, tidak mengunduh |
| Build tanpa cgo | gagal saat buka SQLite (penyebab K5 terbukti) |

---

## Batasan

- **Tidak ada container runtime** (docker/podman/colima) di mesin ini. Perbaikan Dockerfile/entrypoint (K5, K6) tidak dijalankan sebagai container; K5 dibuktikan pada level binary, K6 lewat pembacaan alur mount + salinan config.
- **Tangkapan layar tidak tersedia** ("Screenshot needs a visible tab"). Verifikasi UI memakai pengukuran DOM dan konsol browser.
- **Halaman 2 daftar user** tidak bisa direproduksi dengan 4 akun di instance uji. Perbaikannya diverifikasi dari bentuk parameter yang dikirim dan tipe yang diterima axios.
- **Share `bjcS1X`** (password tidak diketahui) belum diuji; pemiliknya perlu memverifikasi.
- **`write_timeout`** belum terbukti memotong unduhan: uji 10 MB pada loopback selesai utuh.
