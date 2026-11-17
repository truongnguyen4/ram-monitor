# RAM Monitor

Real-time monitor of the RAM in use on an Android device. It runs on a PC (Windows / Linux) and talks to the device through `adb`.
When started, it opens a dashboard in the browser at `http://localhost:8080`.

## Requirements

- `adb` (Android SDK platform-tools). The tool looks for adb in this order: the `-adb` flag, `PATH`, `ANDROID_HOME`/`ANDROID_SDK_ROOT`,
  the folder of the executable, `~/Android/Sdk` (Linux) or `%LOCALAPPDATA%\Android\Sdk` (Windows).
- USB debugging enabled on the device. Works with `user` builds, root is not needed.
- Go (>= 1.22) is only needed to build the tool yourself.

## Run

```sh
# Linux
./ram-monitor-linux-amd64

# Windows
ram-monitor-windows-amd64.exe
```

If only one device is connected, the tool selects it and starts recording right away. With several devices, select one in the dashboard or use `-serial`.
Press `Ctrl+C` in the terminal window to stop.

### Comparing two devices

Run one instance per device. Each instance has its own dashboard, session and CSV files
(file names contain the serial, so instances can share the same `-out` directory):

```sh
./ram-monitor-linux-amd64 -serial <serial-A>     # http://localhost:8080
./ram-monitor-linux-amd64 -serial <serial-B>     # 8080 is busy -> http://localhost:8081
```

When `-port` is not given and 8080 is busy, the next free port up to 8099 is used and printed in the terminal.
With an explicit `-port`, a busy port is an error. The browser tab title shows the device model and serial.
Components are saved per dashboard address (browser storage), so add the same components in each tab to compare them.

| Flag | Default | Description |
|---|---|---|
| `-serial` | (auto) | Device serial |
| `-port` | `8080` | Dashboard port (listens on 127.0.0.1 only) |
| `-interval` | `1s` | `/proc/meminfo` polling interval |
| `-proc-interval` | `10s` | `dumpsys meminfo` polling interval (process table) |
| `-top` | `0` | Limit the process table to the N largest rows after filtering (`0` = show all processes) |
| `-threshold` | `80` | Warning threshold (% of MemTotal) |
| `-out` | `ram-monitor-data` | Output directory for CSV files |
| `-adb` | | adb path |
| `-no-browser` | `false` | Do not open the browser automatically |

## Metrics

**RAM in use** can be calculated in two ways, selected with the toggle in the dashboard:

- **Kernel** (default): `MemTotal − MemAvailable` from `/proc/meminfo`, updated every second.
- **Android (dumpsys)**: the `Used RAM` line of `dumpsys meminfo` (used PSS + kernel), updated every 10 seconds.

**The whole-session chart** splits the RAM in use into stacked layers:

| Layer | Formula (`/proc/meminfo`, kB) |
|---|---|
| Anon (process) | `AnonPages` |
| Shmem | `Shmem` |
| Kernel | `SUnreclaim + KernelStack + ShadowCallStack + PageTables + VmallocUsed + Percpu` |
| zram | `mem_used_total` in `/sys/block/zram0/mm_stat` (real RAM used by the compressed pool) |
| Other (DMA-BUF, GPU…) | The rest = Used − the layers above (DMA-BUF, GPU/driver memory, non-reclaimable cache…) |

**The process table** takes PSS and the OOM group from `dumpsys meminfo`. The Swap column reads `VmSwap` in `/proc/<pid>/status`.
The Δ column is the PSS change since the process was first seen in the session.
The table can be filtered by group (the list shows each group's total PSS and process count) and by process name.
The table lists all processes, sorted by PSS (it scrolls inside the card; the header and the Total row stay visible).
Filters apply to all processes; with `-top N` the table shows only the N largest matching rows.
The first row of the table is the **Total** of PSS, Swap and Δ over every process that matches the filters
(all processes when no filter is set), also when `-top` hides some rows.
Swap is the uncompressed size swapped out (`VmSwap`); the real RAM it takes in zram is smaller (see the zram card).

### Components

The **Components** card, to the right of the process table, compares groups of processes.
Set a filter in the table (a group, a name, or both), then click **+ Add current filter**: the filter becomes a component,
drawn as a horizontal bar (one row per component) whose length is the total PSS of the matching processes
at the latest `dumpsys` poll. Bars are scaled to the largest component and update at every poll.

- Up to 8 components. Each row shows the current total and its change since the start of the session
  (the server keeps the PSS of every process at every poll, so the change is right also after a reload).
- Hover a component's name to see how many processes match; click it to apply its filter to the table; click × to remove it.
- Name filters are case-insensitive substrings, as in the table. A process that changes group (e.g. Cached → Foreground)
  counts in the component of the group it is in at each poll.
- Components are saved in the browser (localStorage), so they stay after a reload or a restart of the tool.

### Process groups

The Group column is the OOM adjustment group printed by `dumpsys meminfo` ("Total PSS by OOM adjustment").
The tool does not define these groups itself. A lower oom_adj means a more important process, which is harder to kill.
Names come from `DUMP_MEM_OOM_LABEL` in `ActivityManagerService.java`, adj values from `ProcessList.java`.

| Group | oom_adj | Meaning | Killed by lmkd |
|---|---|---|---|
| Native | −1000 | C/C++ processes started by init: HALs, daemons, surfaceflinger, zygote | No |
| System | −900 | `system_server` (framework core) | No |
| Persistent | −800 | Apps with `android:persistent="true"`; always running, restarted if they die | No |
| Persistent Service | −700 | Processes bound by a persistent process or by system | Almost never |
| Foreground | 0 | The app the user is currently interacting with | Only under extreme memory pressure |
| Visible | 100 | Visible but not on top, or bound by the foreground app | Very rarely |
| Perceptible (Medium, Low) | 200–250 | Noticeable without being visible: music playback, foreground service, active keyboard | Rarely |
| Backup | 300 | Running a backup or restore | Rarely |
| Heavy Weight | 400 | Legacy heavy-weight app, almost unused nowadays | Yes |
| A Services | 500 | Recently started services | Yes |
| Home | 600 | Launcher when not in foreground | Yes |
| Previous | 700 | The app the user just left | Yes |
| B Services | 800 | Long-running services with lowered priority | Yes, early |
| Cached | 900–999 | Idle background apps kept to relaunch faster | First to be killed |

A process can move between groups over time, e.g. Foreground → Previous → Cached after the user leaves an app.
Native, System and Persistent are a fixed baseline that is never reclaimed. Cached is reclaimed by the system when needed,
so a large Cached group at idle does not mean the device is short of RAM.

## Output files

Each session creates two files in the `-out` directory:

- `ram_<serial>_<time>.csv`: one line per sample (Used, layers, zram, swap, Android Used), in kB.
- `ram_<serial>_<time>_procs.csv`: PSS, group and swap of every process at every `dumpsys` poll (all processes, not only the `-top` rows).

Data is written continuously while the tool runs, so nothing is lost if the tool is closed unexpectedly. The **Export CSV** button downloads the first file.
While **Paused**, no new samples are recorded.
The **Restart** button clears the displayed data (chart, elapsed time, Δ column) and starts a new session on the same device,
writing a new pair of CSV files; the files of the previous session are kept.

## Build

```sh
./build.sh        # creates dist/ram-monitor-linux-amd64 and dist/ram-monitor-windows-amd64.exe
go test ./...     # runs the parser unit tests
```

On Windows you can build directly: `go build -o ram-monitor.exe .`

## Layout

```
main.go                       command-line flags, starts the server, opens the browser
internal/adb                  finds adb, runs commands, keeps a shell session open
internal/collector            reads /proc/meminfo (one long-lived adb shell) + dumpsys meminfo
internal/session              keeps samples in memory, writes CSV
internal/server               HTTP API + Server-Sent Events + embedded web UI
web/                          dashboard (HTML/CSS/JS, uPlot)
```

The chart library [uPlot](https://github.com/leeoniya/uPlot) v1.6.31 (MIT) is bundled in `web/vendor`,
so the tool works without Internet access.
