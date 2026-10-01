# XID policy

An XID is an error report from the NVIDIA driver, logged by the kernel (`NVRM: Xid (PCI:…): 79, …`) and exposed by DCGM as `DCGM_FI_DEV_XID_ERRORS`. NVIDIA's XID documentation lists, for each XID, the likely causes: hardware, driver, user application, system memory corruption, bus error, thermal issue, framebuffer corruption.

The **action** column below is this project's choice, derived from those causes. It is not an NVIDIA recommendation. Tune it for your hardware and your tolerance for false positives (`health.DefaultXIDPolicy`).

Source: NVIDIA, *XID Errors*, [docs.nvidia.com/deploy/xid-errors](https://docs.nvidia.com/deploy/xid-errors/index.html).

| XID | Meaning (short) | Action | Why |
|---|---|---|---|
| 13 | Graphics engine exception | none | Most often the application (e.g. out-of-bounds access) |
| 31 | GPU memory page fault | none | Most often the application (illegal address) |
| 43 | GPU stopped processing | none | The application hit a fault; the GPU itself is fine |
| 45 | Preemptive cleanup | none | Follows another error or a killed process; look at what came before |
| 94 | Contained memory error | watch | The affected application must restart; the GPU keeps working |
| 48 | Double-bit ECC error | drain | Uncorrectable memory error; reset clears the volatile state |
| 63 | Row remapping pending | drain | The remap takes effect on the next GPU reset |
| 74 | NVLink error | drain | Link problem; reset and validate before trusting the node |
| 79 | GPU has fallen off the bus | drain | GPU unreachable; needs at least a reset, often a reboot |
| 95 | Uncontained memory error | drain | Other processes may be affected; reset required |
| 119, 120 | GSP RPC timeout / GSP error | drain | Firmware not responding; reset required |
| 64 | Row remapping failure | quarantine | The memory cannot be repaired in place: hardware replacement |
| 92 | High single-bit ECC error rate | quarantine | Degrading memory; repairing in place just delays the next failure |
| others | | watch | Unknown to the policy: record and alert, a human decides |

Other rules, independent of XIDs:

| Signal | Action |
|---|---|
| `DCGM_FI_DEV_ECC_DBE_VOL_TOTAL > 0` | drain |
| `DCGM_FI_DEV_ROW_REMAP_FAILURE = 1` | quarantine |
| `DCGM_FI_DEV_GPU_TEMP ≥ 90 °C` (flag `-temp-cordon`) | cordon only |

The worst action across all GPUs of a node wins: one GPU with XID 64 quarantines the whole node, because Kubernetes schedules GPUs per node.

## Known gaps

- `DCGM_FI_DEV_XID_ERRORS` holds the *last* XID only. Two different XIDs between two scrapes means the first is missed. Production setups also tail the kernel log or use DCGM health watches.
- Application XIDs (13, 31, 43) that keep repeating on one node can still point at hardware. The policy has no rate rule for them yet.
- The thermal threshold is a single number. Real limits depend on the GPU model (`nvidia-smi -q -d TEMPERATURE` shows the slowdown and shutdown temperatures).
