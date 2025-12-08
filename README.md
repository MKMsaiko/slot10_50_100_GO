# slot10_50_100_GO

- Slot simulator demo

- 模擬遊戲數據，輸出各項統計資料

- 遊戲規則與程式流程請見程式檔頭註解

## 環境需求
- Windows 10/11
- Go 1.24

## 開發工具(建議)
- Visual Studio Code
  - 擴充：Go (golang.go)

## Build
- go build -o slot10_50_100.exe .

## Run (example)
- .\slot10_50_100.exe 或 go run .\slot10_50_100.go

## 附註
- 本專案中之.go檔無外部依賴，迅速試跑可直接貼進VScode/VS或其他編譯軟體測試

## 模擬器輸出示意圖
<p align="center">
  <img src="image/10_50_100Sim.png" width="700" alt="模擬輸出截圖">
  <br><sub>RTP、獎項分佈、峰值摘要等</sub>
</p>

<p align="center">
  <img src="image/10_50_100Win.png" width="300" alt="模擬輸出截圖">
  <br><sub>大獎詳細分層</sub>
</p>

<p align="center">
  <img src="image/10_50_100Stat.png"  width="400" alt="模擬輸出截圖">
  <br><sub>與Excel試算之結果驗證</sub>
</p>
