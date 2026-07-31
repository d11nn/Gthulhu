# Goal Prompt 8h1m 執行問題復盤

## 文檔資訊

- 復盤範圍：從啟動 `/home/ubuntu/goal-prompt.md` 到執行約 8 小時 1 分後暫停延伸實作。
- 復盤時間：2026-07-29 UTC。
- 主要事實來源：
  - `/home/ubuntu/goal-prompt.md`
  - `/home/ubuntu/aether-gthulhu-integration-facts.md`
  - `/home/ubuntu/gthulhu-aether5GC-proposal.md`
  - `/home/ubuntu/Gthulhu` 提交紀錄
  - `/home/ubuntu/aether-onramp-gthulhu-addon` 提交紀錄
  - `/home/ubuntu/aether-onramp` 部署分支與 node1 即時只讀檢查
  - Ubuntu 25.04 release notes：https://documentation.ubuntu.com/release-notes/25.04/
  - Ubuntu 24.04 LTS release notes：https://documentation.ubuntu.com/release-notes/24.04/

## 結論摘要

這 8h1m 並不是完全「解決不了」。目前狀態是：

1. Gthulhu addon、Scheduling-Aware Scaling Controller、Manager 策略傳遞、Helm 整合、Aether 指標查詢、目標 Pod scheduling metrics、映像交付流程都已完成程式碼與離線驗證。
2. RKE2、Aether5GC、AMP ROC、Rancher Monitoring 與 11 個 Aether ServiceMonitor 已實際部署。
3. 真正阻止 goal 完成的是線上 SD-Core MongoDB probe 重啟迴圈造成的主機過載。這使 MongoDB data replicas 與 metrics-server 不健康，不適合再加入 Gthulhu 工作負載。
4. MongoDB TCP probe 穩定化修復已提交、渲染驗證且通過 live server dry-run，但實際 rollout 屬於叢集寫入，尚未取得明確核准，因此沒有套用。
5. 在 rollout 前，Gthulhu scheduler/API 映像不能安全地實際 build/import，Gthulhu 也不能安裝，所以同叢集 runtime 驗收、scheduling metrics、策略到 intent 的線上驗證仍無法完成。

換句話說，剩餘問題不是缺少修復方案，而是：

- 叢集先決條件不健康。
- 修復先決條件需要受控的線上寫入核准。
- goal 的最後驗收必須依賴真實 runtime，不能用單元測試或 Helm render 取代。

## 原始完成定義

`goal-prompt.md` 要求的不只是產生 YAML 或程式碼，而是：

1. Gthulhu 必須部署到 Aether OnRamp 建立的同一個 RKE2 cluster。
2. scheduler DaemonSet、Manager、sidecar 與 scaling controller 必須實際運行。
3. Prometheus 必須能發現 Aether service metrics 與 Gthulhu scheduling metrics。
4. controller 必須用 service KPI 與 scheduling pressure 兩類訊號實作 `Normal`、`Congested`、`Recovery`。
5. scaling policy 與 Gthulhu scheduling strategy 必須能在線上更新。
6. 測試 `ScheduleStrategy` 必須能傳遞成 `ScheduleIntent`。
7. 不能使用 MicroK8s 作為最終部署路徑。

因此，只完成程式碼、測試與 chart render，仍不能宣告 goal 完成。

## 暫停時的線上狀態

2026-07-29 06:30 UTC 的只讀快照：

- node1：8 vCPU。
- load average：`82.22, 87.21, 86.52`。
- CPU PSI `some`：`90.19/90.20/91.62%`，`full=0`。
- `mongodb-0`：`0/1`，121 次 restart，快照前 30 秒又重啟。
- `mongodb-1`：`0/1`，208 次 restart。
- `mongodb-arbiter-0`：`1/1`。
- `rke2-metrics-server`：`0/1`，9 次 restart。
- `atomix-consensus-controller`：已恢復 `1/1`。
- SD-Core Helm release：revision 2，`deployed`。
- Gthulhu Helm release：不存在。
- KEDA CRD：不存在。

這表示 API 顯示 release `deployed`，但 workload prerequisite 並不健康。

## 新手版：MongoDB 為什麼反覆重啟

### 一句話答案

MongoDB 不是自己崩潰，而是健康檢查在主機 CPU 嚴重壅塞時超時。Kubernetes 因此誤以為 MongoDB 卡死，主動把它關掉再啟動；重啟又增加主機負擔，下一輪健康檢查更容易超時，最後形成反覆重啟的迴圈。

### 2026-07-31 再次只讀確認

兩天後問題仍在：

- `mongodb-0` 已累積 222 次 restart。
- `mongodb-1` 已累積 463 次 restart。
- 兩個 Pod 都觀察到從短暫 `1/1 Running` 變回 `0/1 Running`。
- 8 vCPU node 的 load average 約為 `56.27, 49.67, 46.42`。
- CPU PSI `some avg10=86.17%`，表示在最近 10 秒內，至少有一個工作約 86% 的時間因搶不到 CPU 而等待。

`Running` 只代表容器程序還存在，不等於應用程式已通過健康檢查。因此 `0/1 Running` 的意思是「MongoDB 容器在跑，但 Kubernetes 目前不認為它 Ready」。

### Kubernetes 做了兩種不同的檢查

目前 MongoDB data replica 的檢查設定是：

| 檢查 | 頻率與期限 | 失敗後的動作 |
|---|---|---|
| readiness probe | 每 10 秒執行一次，最多等 5 秒 | 將 Pod 標成 `0/1`，暫時不視為可服務；不會直接重啟 |
| liveness probe | 每 20 秒執行一次，最多等 10 秒，連續失敗 6 次 | kubelet 判定容器失去生命跡象並重啟它 |

所以看到 readiness timeout 時，先發生的是 Pod 變成 `0/1`；真正按下「重啟」按鈕的是後續持續失敗的 liveness probe。

Kubernetes event 已直接寫出這條因果：

```text
Readiness probe failed:
command timed out: "/bitnami/scripts/readiness-probe.sh" timed out after 5s

Liveness probe failed:
command timed out: "/bitnami/scripts/ping-mongodb.sh" timed out after 10s

Container mongodb failed liveness probe, will be restarted
```

在保留的 event 時間窗內，`mongodb-1` 有 2,784 次 readiness timeout、693 次 liveness timeout，以及 19 次因 liveness 失敗而重啟。Pod 顯示的 463 次 restart 是這個 Pod 從建立至今的累計值；event 只保留近期資料，所以兩個數字不應直接相等。

### 問題在於每次檢查都會啟動一個 `mongosh`

liveness script 的核心內容是：

```bash
exec mongosh --port 27017 --eval "db.adminCommand('ping')"
```

readiness script 也會啟動 `mongosh`，再執行 `db.hello()` 判斷節點是不是 primary 或 secondary。

這不是單純查看 27017 port 是否開啟。`mongosh` 是一個完整的 Node.js 命令列程式；每次 probe 都要建立新程序、載入 runtime、連線 MongoDB、執行指令，再把結果交回 kubelet。兩個 MongoDB data replica 都會各自反覆執行這些程序。

正常負載下，這個檢查可能很快完成。但目前 8 vCPU node 上有大量工作在排隊，MongoDB 容器本身又受 CPU limit 約束。MongoDB 可能仍然活著、仍能處理請求，只是新啟動的 `mongosh` 沒有在 5 秒或 10 秒期限內取得足夠 CPU 完成檢查。

MongoDB 當下的 current log 也看得到來自 `127.0.0.1`、client 為 `mongosh 2.5.6`、platform 為 Node.js `v20.19.4` 的連線。這和 probe script 的行為一致。

### MongoDB log 證明它是被外部關掉，不是自己崩潰

`mongodb-1` 上一次重啟前的 MongoDB log：

```text
"msg":"Received signal","attr":{"signal":15,"error":"Terminated"}
"msg":"Signal was sent by kill(2)","attr":{"pid":0,"uid":0}
"msg":"Entering quiesce mode for shutdown","attr":{"quiesceTimeMillis":15000}
"msg":"WiredTiger closed"
"msg":"mongod shutdown complete"
"msg":"Shutting down","attr":{"exitCode":0}
```

逐行翻成白話：

1. `signal: 15` 是 `SIGTERM`，意思是外部要求 MongoDB 正常關機。
2. `kill(2)` 表示關機訊號來自作業系統層級，不是 MongoDB 自己丟出 fatal error。
3. `quiesce mode` 表示 MongoDB 先停止接新工作，準備有秩序地退出。
4. `WiredTiger closed` 表示儲存引擎已正常關閉。
5. `exitCode: 0` 表示程序認為這次退出成功，不是 crash。

Kubernetes 的 container 狀態也吻合：

```text
Last State:  Terminated
Reason:      Completed
Exit Code:   0
```

因此目前證據不支持「MongoDB 資料損壞後自行崩潰」或「MongoDB 程式發生 fatal error」。證據支持的是：liveness probe 超時後，kubelet 對容器送出 `SIGTERM`；MongoDB 完成正常關機，之後由 Kubernetes 建立新的 container。

### 完整的反覆重啟迴圈

1. MongoDB 啟動並可處理工作。
2. Kubernetes 持續啟動新的 `mongosh` 做 readiness 與 liveness 檢查。
3. 主機 CPU 長時間壅塞，probe 程序無法在 5 秒或 10 秒內完成。
4. readiness 失敗，Pod 先變成 `0/1`。
5. liveness 連續失敗，kubelet 判定 MongoDB 卡死。
6. kubelet 送出 `SIGTERM`；MongoDB checkpoint、關閉 WiredTiger，並以 exit code 0 結束。
7. Kubernetes 重啟 container；啟動、儲存恢復、container runtime 與後續 probes 又消耗 CPU。
8. 主機壓力沒有消失，下一輪 probe 再度超時，回到第 4 步。

這是一個「健康檢查超時造成重啟，重啟又加重超時」的回饋迴圈。它不代表每次 restart 前 MongoDB 都已不能使用；它代表目前的健康檢查成本與期限不適合這台已過載的單節點主機。

`mongodb-1` 的 restart 數比 `mongodb-0` 高，可能和每次檢查發生的時間、當時 replica role 或瞬間負載有關。現有證據足以證明兩者都受到相同的 probe timeout 機制影響，但不足以斷言兩者次數差異的唯一原因。

### 已準備但尚未套用的修復

已完成的修復會把 `exec + mongosh` probe 改為 TCP 27017 檢查，並把 data replica CPU limit 提高到 1 core：

- isolated addon commit：`cf081af`
- deployment branch commit：`380940d`

TCP probe 只需確認 MongoDB 是否正在監聽 port，不必每次啟動完整的 `mongosh`，因此能直接移除目前最明顯的 probe 額外負擔。代價是 TCP probe 只能證明程序有監聽，不能判斷節點目前是 primary、secondary，或 replica set 邏輯是否完全健康。

對目前要先穩定單節點 lab 的情境，這是務實的第一步；若是正式多節點環境，仍應在主機資源足夠後設計能判斷 replica role、但不會因短暫 CPU 壅塞就殺掉資料庫的 readiness 檢查。這份修復已通過 render、結構斷言與 server-side dry-run，但因實際套用屬於叢集寫入，目前沒有執行。

## 為何不能直接繼續安裝 Gthulhu

### 1. 會把已過載的單節點推向更差狀態

Gthulhu 會新增至少：

- privileged scheduler DaemonSet
- scheduler sidecar
- Manager
- scaling controller
- Gthulhu MongoDB
- monitoring scrape workload

目前 8 vCPU node 的 CPU PSI 約 90%，MongoDB probes 已持續觸發 container exec、`mongosh`、`runc` 與 restart。此時增加工作負載會降低恢復機率，也會讓任何 Gthulhu 問題無法和既有過載問題區分。

### 2. 不符合 goal 的 acceptance gate

提案已把「SD-Core MongoDB replicas 與 metrics-server 恢復 Ready」列為 Gthulhu 驗收前置條件。跳過此 gate，即使 Helm install 成功，也不能證明整合可運作。

### 3. 修復需要叢集寫入核准

可執行修復已存在：

- isolated addon commit：`cf081af`
- deployment branch commit：`380940d`

修復內容：

- MongoDB data replicas 與 arbiter 的 liveness/readiness 由 `exec + mongosh` 改成 TCP 27017。
- data replica CPU limit 設為 1 core。
- 保留現有 memory 與 ephemeral storage 限制。

已通過：

- Ansible syntax check
- 兩份真實 Jinja values render
- YAML 結構化斷言
- SD-Core 4.1.4 完整 chart render
- live API `helm upgrade --dry-run=server --hide-secret`

尚未執行的唯一關鍵步驟是實際 Helm rollout。先前 live command 在執行前被 approval layer 拒絕，後續也未收到明確的線上寫入核准。

## 問題分類

### A. 目前阻塞 goal 的問題

| ID | 問題 | 根因 | 已完成處置 | 為何仍未完成 |
|---|---|---|---|---|
| B1 | MongoDB data replicas 長期 `0/1` | Bitnami probe 每次啟動 `mongosh`，在 CPU throttling 下 timeout，形成 probe、restart、額外 runtime 負載的回饋迴圈 | TCP probe 修復已提交並完成 dry-run | 實際 rollout 是 cluster write，尚未獲得核准 |
| B2 | node1 極端 CPU pressure | 8 vCPU 同時承載 RKE2、SD-Core、ROC、Monitoring；MongoDB probes 加劇 system/softirq 與 run queue | 找出 probe 根因並準備低成本 probe 修復 | 需先套用 B1，才能觀察負載是否下降 |
| B3 | metrics-server `0/1` | 不是固定網路阻斷；直接 kubelet 與 API proxy 證明路徑可達，症狀較符合過載造成的 timeout | 完成網路路徑排除 | 需等待主機壓力恢復後再驗證 |
| B4 | Gthulhu 尚未部署 | 安裝前置的 SD-Core 健康 gate 未通過 | addon、preflight、values、chart、controller 均已準備 | 不應在 B1-B3 未恢復時增加 workload |
| B5 | scheduling metrics 尚無 live series | scheduler 與 target-scoped `PodSchedulingMetrics` 尚未上線 | CR、ServiceMonitor、watcher、PromQL 都已離線驗證 | 必須先部署 scheduler 才能產生 series |
| B6 | strategy-to-intent 未做 live 驗收 | Manager/controller 尚未上線 | Manager endpoint、intent create/replace、Decision Maker path 已由測試覆蓋 | 必須先完成 Gthulhu install |
| B7 | KEDA live autoscaling 未驗證 | cluster 沒有 `scaledobjects.keda.sh` CRD | addon 可偵測缺失並安全跳過；combined PromQL 與 ScaledObject render 已驗證 | 若要完整驗證 KEDA/HPA，仍需另行核准安裝 KEDA |

### B. 部分解決或保留的風險

| ID | 問題 | 現況 | 保留原因 |
|---|---|---|---|
| P1 | SD-Core shared-CA helper 不相容 Helm 3.20 | 已用 reviewed temporary chart patch 成功部署 revision 2 | upstream 4.1.2/4.1.4 helper 仍有 `b64dec`/`buildCustomCert` 契約錯誤，未形成 upstream fix |
| P2 | node1 使用 Ubuntu 25.04 | RKE2/Aether 實際可運行 | OnRamp 文件只支援 22.04/24.04；25.04 已 EOL，不適合作為可重現基線 |
| P3 | scheduler/API 實際映像 build/import 未執行 | Ansible、tag、save/import、Helm image selection 已驗證 | build/import 是 node1 寫入與長時間網路/CPU 工作，應在 SD-Core 恢復後執行 |
| P4 | 正式 thresholds 未校準 | metric names 與 PromQL 已 live 驗證 | 需要代表性 5GC 流量與實際 scheduling series 才能校準 |
| P5 | OnRamp commits 未 push | isolated 與 deployment branches 都有完整本機 commits | 不存在允許的 `d11nn/aether-onramp` repo；依限制不能 push 到 `opennetworkinglab/*` |
| P6 | 文檔無 commit | proposal、facts 與本復盤位於 `/home/ubuntu` | 這些路徑不屬於任何 Git repo |
| P7 | sandbox helper 持續失效 | 可用 escalated read/validation 與 guarded Python edit 繞過 | `bwrap: loopback: Failed RTM_NEWADDR` 是執行環境問題，不是 repo 程式碼問題 |

### C. 已解決的主要問題

| ID | 問題 | 根因 | 解法與結果 |
|---|---|---|---|
| R1 | 本機找不到 RKE2 kubeconfig | kubeconfig 位於 remote node1，不在 control host | 改用 OnRamp inventory 經 SSH 在 node1 執行 kubectl/Helm |
| R2 | SSH host-key verification failure | node1 未被 local known_hosts 信任 | restart 後連線恢復，Ansible ping 與 live install 可用 |
| R3 | RKE2 無法啟動 kubelet | MicroK8s `kubelite` 佔用 10248/10250 | `snap stop --disable microk8s`，RKE2 立即啟動 |
| R4 | CoreDNS 無法連到 `10.43.0.1` | kernel/iptables 混有 MicroK8s Calico 舊規則 | 在 MicroK8s disabled 狀態 reboot，RKE2 網路恢復 |
| R5 | SD-Core 初次 install timeout | Kafka 與 UPF 冷拉映像分別約 5m48s/6m03s | 等待 pull 收斂並延長可靠部署路徑 |
| R6 | ROC/Monitoring Helm timeout | 預設 5m 小於 Grafana/Prometheus 冷拉時間 | timeout 改為 15m；後續 reapply 無 rescue/retry |
| R7 | ROC gNMI `DeadlineExceeded` | 初始 transaction 約 16s，預設 timeout 10s | 改為 30s，live transaction 成功 |
| R8 | ROC model loader 沒送出正確 request | YAML header 型別錯誤，stdin path 行為不符預期 | header 改成 string，改用 remote file redirection + `kubectl exec -i` |
| R9 | Go/Helm 被誤判不存在 | binary 不在 shell PATH | 使用 `/usr/local/go/bin/go` 與絕對 Helm path |
| R10 | full Go test 缺 frontend embed asset | `api/web/dist` 未建置 | 建立 ignored minimal validation asset，`go test ./...` 通過 |
| R11 | executionTime 被 render 成科學記號 | Helm 對大數值直接 render | 使用 int64 render，JSON 保持整數 |
| R12 | 直接 upsert SchedulingStrategy 無法保證 intent 傳遞 | 只有 Manager service 會建立/更新 `ScheduleIntent` 並送 Decision Maker | 新增 authenticated Manager internal upsert endpoint |
| R13 | 兩個 KEDA trigger 破壞雙訊號條件 | KEDA 任一 trigger 即可觸發 scale | 每個 target 改用單一 combined boolean PromQL |
| R14 | priority 方向註解錯誤 | 實際 Qumun 契約是數字越小 priority 越高 | 修正註解並驗證 `0-20`、baseline 10、boosted 2 |
| R15 | 預設 Aether metric names 不存在 | chart 假設和 live SD-Core 4.1.2 不符 | live discovery 後改用 `n4_messages_total` 與 `upf_bytes_count` |
| R16 | scheduler 不會收集 SMF/UPF PID | `monitorAll=false` 且沒有 `PodSchedulingMetrics` | chart/addon 產生兩個 target-scoped CR |
| R17 | scheduler restart 後可能漏掉既有 CR | watcher 只 `Watch()`，沒有 initial `List()` | list existing resources，再從 list `resourceVersion` watch |
| R18 | official API/scheduler image 不含本次新程式 | published `develop` 不能保證包含 controller/watcher | addon build 本機 API 與 scheduler，使用 source commit tag 匯入 RKE2 |
| R19 | scheduler Dockerfile Go 版本過舊 | Dockerfile 1.22.10，root/API modules 要求 1.24.5 | commit `d8aa5b3` 對齊 Go 1.24.5 |
| R20 | scheduler image 基底 EOL | builder/runtime 使用 Ubuntu 25.04 | commit `8db2f31` 改成 Ubuntu 24.04 LTS |
| R21 | monitor tests link 失敗 | 直接 `go test` 缺 repo 的 libbpf CGO flags | 使用 `make test-monitor`，所有 monitor tests 通過 |
| R22 | dynamic fake watcher test 引入缺少的 module | `dynamic/fake` 需要未列入 go.sum 的 json-patch | 抽出純 helper 測 snapshot replacement，避免不必要 dependency |

## 8h1m 主要花費在哪裡

### 1. 工作範圍包含完整平台恢復，不只是 Gthulhu 程式碼

重啟後需要重新確認並實際建立：

- RKE2
- SD-Core
- AMP ROC
- ROC model load
- Rancher Monitoring
- ServiceMonitors
- Prometheus target/metric discovery

這些是 goal 的實際 prerequisite，不完成就無法做 Gthulhu live validation。

### 2. 冷映像拉取本身耗時且造成多次 Helm timeout

觀察到：

- Kafka pull：約 5m48s
- UPF BESS pull：約 6m03s
- Prometheus pull：約 8m57s
- Grafana pull：約 13m20s

預設 Helm timeout 只有 2m30s 或 5m，導致第一次執行表面失敗，但資源仍在背景收斂。每次都必須先辨識是「真正 chart 錯誤」還是「cold pull 超時」，再安全重試。

### 3. 遇到多個互不相關的 upstream/environment 問題

同一輪中先後遇到：

- MicroK8s port 與 iptables 汙染
- SD-Core CA helper bug
- AMP Helm timeout
- ROC gNMI timeout
- model loader stdin/header 問題
- MongoDB probe feedback loop
- sandbox `bwrap` 失效
- local/remote 工具與 kubeconfig 路徑差異

這些問題不能用同一個修補解決，需要逐層取得證據。

### 4. 叢集過載讓每個遠端操作都更慢

node1 的 run queue、system CPU、softirq 與 CPU PSI 長期偏高。Ansible、kubectl、probe、container runtime 與 Helm 操作都受影響，且 timeout 結果需要額外判斷。

### 5. goal 要求每個 vertical slice 都驗證並更新 facts

每個 major milestone 都執行：

- focused tests
- full regression 或 Helm lint/template
- structured YAML assertion
- Git tree/status check
- commit
- 僅允許的 `d11nn/Gthulhu` push 與 remote tree 驗證
- facts/proposal 更新

這增加耗時，但避免在最後才發現 metric name、watcher、image 或 priority 契約錯誤。

### 6. Git push 有額外限制

- 只能 push 到 `d11nn/*`。
- 不能 push 到 `Gthulhu/*` 或 `opennetworkinglab/*`。
- control host 的直接 HTTPS git push 沒有 credentials。
- Gthulhu 改用 GitHub connector 建 blob/tree/commit/ref，再驗證 remote tree。
- OnRamp 沒有 `d11nn/aether-onramp`，所以只能保留本機 commits。

## 已完成的程式碼里程碑

### Gthulhu

| 本機 commit | `d11nn/Gthulhu` remote commit | 內容 |
|---|---|---|
| `385ff67` | `51323f6` | Aether 5GC scaling controller |
| `2736d55` | `badd9626` | scheduler priority profile validation |
| `3124ed2` | `5099ca9a` | live Aether metric names |
| `8175442` | `f96364ab` | target-scoped scheduling metrics resources |
| `e7f0026` | `4d52ec3d` | restart-safe list-then-watch |
| `d8aa5b3` | `1ba4c193` | Go 1.24.5 image toolchain |
| `8db2f31` | `4d0162b6` | Ubuntu 24.04 LTS image base |

最終 Gthulhu local tree 與 `d11nn/Gthulhu` remote tree 已驗證一致。

### Aether OnRamp

| isolated addon | deployment branch | 內容 |
|---|---|---|
| `72251bf` | `4df0715` | Gthulhu addon skeleton/install/preflight |
| `76bbae7` | `e75260e` | ROC cold-start robustness |
| `e95d5b5` | `2ba759f` | model content-type scalar |
| `ffbeb8c` | `226ba99` | remote-file model streaming |
| `f38fd7d` | `eaa67d3` | monitoring Helm timeouts |
| `d03e0fe` | `9b76345` | first MongoDB resource/probe timeout mitigation |
| `9c83030` | `b1c6ff7` | local Gthulhu API image delivery |
| `6a2da81` | `700e804` | verified RKE2 CRI/containerd tools |
| `cf081af` | `380940d` | MongoDB TCP probes |
| `cad3af0` | `a141ad3` | explicit scheduling metric targets |
| `a7b857a` | `e2c6c46` | local scheduler image delivery |

這些 commit 沒有 push，原因是沒有允許的 `d11nn/aether-onramp` remote。

## 尚未執行的最短完成路徑

### Gate 1：核准並套用 SD-Core TCP probe 穩定化

只套用已驗證的：

- `cf081af` / `380940d`
- 不同時安裝 Gthulhu
- 不同時安裝 KEDA

### Gate 2：觀察 prerequisite 恢復

至少確認：

- `mongodb-0`、`mongodb-1` 變成 `1/1`
- restart count 不再持續增加
- `rke2-metrics-server` 變成 `1/1`
- CPU PSI 與 load 明顯下降
- SD-Core 關鍵 NFs 保持 Ready

若沒有恢復，應停止 Gthulhu install，繼續處理 MongoDB/CPU 根因。

### Gate 3：build/import 本機映像

- scheduler：`docker.io/library/gthulhu-scx-aether:aether5gc-8db2f31c2201`
- API：`docker.io/library/gthulhu-api-aether:aether5gc-8db2f31c2201`
- import 到 RKE2 containerd `k8s.io` namespace
- 用 `crictl inspecti` 驗證

### Gate 4：安裝 Gthulhu addon

- 執行 `make aether-gthulhu-install`
- 驗證 scheduler DaemonSet、Manager、MongoDB、sidecar、controller
- 驗證 logs、health、ServiceMonitor

### Gate 5：完成 live acceptance

- Prometheus 發現 Gthulhu scheduling metrics
- SMF/UPF `PodSchedulingMetrics` 有 series
- 測試 `ScheduleStrategy` 產生/更新 `ScheduleIntent`
- controller 可達 Prometheus 與 Manager
- controller metrics 顯示 state/action
- 視需求安裝 KEDA，再驗證 ScaledObject/HPA

## 需要的決策

1. 是否核准先套用 SD-Core TCP probe 穩定化。
2. 是否建立 `d11nn/aether-onramp`，讓已提交的 OnRamp 變更可依限制 push。
3. 是否把 node1 重建到 OnRamp 支援的 Ubuntu 24.04 LTS；目前 25.04 適合短期 lab 驗證，不適合可重現基線。
4. 完整驗收是否要求本次同時安裝 KEDA，或先以 monitoring-only mode 驗證 Gthulhu controller。

## 最終判斷

目前不能宣告 goal 完成，因為關鍵 runtime acceptance 尚未發生。

目前也不應判斷為「技術上無法解決」，因為：

- MongoDB 修復已完成到 live server dry-run。
- Gthulhu controller/addon/image path 已完成到離線驗證。
- 剩餘路徑明確且可排序。

準確狀態是：

> 實作大致完成，線上 prerequisite 因 MongoDB probe feedback loop 與 CPU pressure 不健康；修復等待受控 rollout 核准，因此 Gthulhu 部署與 end-to-end 驗收尚未開始。
