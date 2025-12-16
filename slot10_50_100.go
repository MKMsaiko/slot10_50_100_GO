/*
遊戲規則：
Line Game : 25 lines  window : 3*5
線獎由左到右，同符號達3、4、5連線且符合線圖，即有贏分
主遊戲中出現3個以上任意位置的Scatter觸發免費遊戲x5
免費遊戲中出現3個任意位置的Scatter可再觸發，無上限
主遊戲出現｛3，4，5｝個Scatter觸發之免費遊戲，該免費遊戲贏分分別｛x10，x50，x100｝
免費遊戲中再觸發之免費遊戲則依最初主遊戲觸發時之倍率計算
Scatter出現在3-5轉輪，Wild出現在2-5轉輪，可替代除S外之任意符號
該遊戲有兩個賠率表，皆為單線押注1時之表現

程式流程：
依處理器thread數 → 分數個worker → 各自做以下1-7 → 彙整輸出

1.轉窗（主遊戲）

	呼叫：spinWindow(rng, reelsMG, &w)
	對 5 軸各抽停點，寫進 window5x3 w.c[5][3]。

2.算主遊戲線獎

	呼叫：evalAllLines(&w, &payMG)
	→ 走 25 條線，逐條呼叫linePay(...) 加總。
	linePay：
		湊到 3/4/5 連就回 pay[target][len-3]。

3.判 Scatter、決定是否進 FG

	呼叫：countScatter(&w) → 數 5×3 窗內 S。
	<3：本把結束（若 mgLine==0，deadSpins++）。
	>=3：進 FG，記 triggerCount 與倍率：
		呼叫：fgMulByScatter(s) → 3S/4S/5S → ×10/×50/×100。

4.跑「一整串」FG

	呼叫：playFG(rng, &w)，回傳：
		spins,base,retri,zeroBatches...

	playFG 的每一轉內部其實就是：
		spinWindow(rng, reelsFG, &w)（換 FG 輪帶）
		win := evalAllLines(&w, &payFG) * betPerLine
		if countScatter(&w) >= 3 { queue += 5; retri++ }
		做 5 轉批次統計

5.把整串 FG 派彩加權、累計

	fgWin := fgBase * mul, spinTotal := mgLine + fgWin...

6.更新單把峰值與分層

	依門檻記 big/mega/super/holy/jumbo/jojo；更新 maxSingleSpin。

7.進度心跳（降低同步成本）

	每 4096 轉做一次 atomic.AddInt64(&spinsDone, 4096)。
*/
package main

import (
	"fmt"
	"log"
	"math"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

/**************
 * 參數
 **************/
var (
	numSpins   int64   = 1000000000       // 轉數(預設為10億轉)
	betPerLine float64 = 0.04             // 線注
	numWorkers         = runtime.NumCPU() // total threads

	excelRTP float64 = 0.965984 // Excel 試算之 RTP，不比較則設負值
)

const numLines = 25

/**************
 * 符號編碼
 **************/
const (
	S9 uint8 = iota // 數列: 0 1 2 3...(8 bits)
	S10
	SJ
	SQ
	SK
	SR
	SF
	SB
	SW
	SS
	NumSymbols
)

/**************
 * 賠率表（整數化） pay[符號][streak-3] = payout
 **************/
// === MG 賠率表 ===
var payMG = func() (p [NumSymbols][3]float64) {
	p[S9] = [3]float64{5, 10, 40}
	p[S10] = [3]float64{5, 15, 50}
	p[SJ] = [3]float64{10, 15, 75}
	p[SQ] = [3]float64{10, 20, 100}
	p[SK] = [3]float64{10, 25, 150}
	p[SB] = [3]float64{15, 50, 300}
	p[SF] = [3]float64{25, 100, 500}
	p[SR] = [3]float64{40, 200, 1500}
	return
}()

// === FG 賠率表 ===
var payFG = func() (p [NumSymbols][3]float64) {
	p[S9] = [3]float64{10, 15, 100}
	p[S10] = [3]float64{10, 25, 125}
	p[SJ] = [3]float64{15, 30, 150}
	p[SQ] = [3]float64{15, 40, 175}
	p[SK] = [3]float64{30, 45, 300}
	p[SB] = [3]float64{30, 100, 800}
	p[SF] = [3]float64{50, 200, 1500}
	p[SR] = [3]float64{100, 500, 3000}
	return
}()

/**************
 * 線圖 25線（0=上,1=中,2=下）
 **************/
var lines = [numLines][5]uint8{
	{1, 1, 1, 1, 1}, {0, 0, 0, 0, 0}, {2, 2, 2, 2, 2}, {0, 1, 2, 1, 0}, {2, 1, 0, 1, 2},
	{0, 0, 1, 2, 2}, {2, 2, 1, 0, 0}, {1, 2, 2, 2, 1}, {1, 0, 0, 0, 1}, {0, 1, 1, 1, 0},
	{2, 1, 1, 1, 2}, {1, 0, 1, 2, 1}, {1, 2, 1, 0, 1}, {0, 0, 2, 2, 0}, {2, 2, 0, 0, 2},
	{0, 2, 2, 2, 0}, {2, 0, 0, 0, 2}, {1, 0, 2, 0, 1}, {1, 2, 0, 2, 1}, {0, 1, 0, 1, 0},
	{2, 1, 2, 1, 2}, {1, 1, 0, 1, 1}, {1, 1, 2, 1, 1}, {0, 2, 0, 2, 0}, {2, 0, 2, 0, 2},
}

/**************
 * 轉輪表（字串→啟動時轉 uint8）
 **************/
// === MG 轉輪表 ===
var reelsMGstr = [][]string{
	// Reel 1
	{"10", "Q", "9", "R", "B", "J", "10", "K", "Q", "10", "J", "Q", "10", "J", "B", "Q", "J", "J", "10", "Q", "9", "Q", "Q", "B", "9", "J", "B", "F", "K", "Q", "K", "B", "B", "Q", "B", "10", "J", "Q", "10", "B", "F", "K", "R", "B", "R", "10", "9", "J", "Q"},
	// Reel 2
	{"9", "K", "J", "9", "Q", "B", "9", "K", "B", "9", "K", "9", "9", "W", "10", "J", "R", "B", "10", "Q", "W", "R", "K", "9", "10", "K", "Q", "K", "B", "F", "K", "R", "K", "Q", "B", "K", "9", "B", "F", "10", "R", "Q", "K", "R", "9", "K", "W", "9", "10", "9"},
	// Reel 3
	{"9", "9", "9", "10", "S", "9", "10", "9", "10", "10", "9", "10", "S", "10", "J", "10", "10", "9", "J", "F", "J", "10", "J", "J", "Q", "J", "R", "Q", "Q", "J", "Q", "F", "K", "K", "B", "B", "J", "F", "F", "J", "B", "K", "R", "R", "F", "F", "R", "W", "9", "10", "J"},
	// Reel 4
	{"9", "9", "10", "9", "9", "J", "9", "Q", "9", "9", "10", "10", "9", "10", "10", "J", "J", "9", "J", "J", "Q", "R", "J", "J", "Q", "Q", "9", "Q", "J", "Q", "W", "W", "W", "W", "K", "F", "K", "Q", "B", "B", "W", "W", "W", "W", "R", "J", "B", "K", "Q", "Q", "B", "Q", "F", "K", "10", "S", "S"},
	// Reel 5
	{"9", "9", "10", "10", "Q", "W", "J", "J", "J", "Q", "Q", "K", "W", "Q", "Q", "10", "Q", "K", "K", "Q", "K", "K", "F", "B", "K", "B", "B", "10", "B", "K", "B", "B", "J", "B", "R", "B", "F", "F", "K", "F", "B", "F", "F", "R", "B", "Q", "W", "F", "B", "10", "S", "S"},
}

// === FG 轉輪表 ===
var reelsFGstr = [][]string{
	// Reel 1
	{"10", "Q", "B", "10", "Q", "J", "9", "B", "9", "Q", "J", "K", "J", "10", "J", "Q", "J", "B", "Q", "K", "Q", "10", "Q", "B", "Q", "R", "B", "K", "J", "Q", "10", "K", "9", "Q", "B", "R", "J", "Q", "10", "B", "Q", "F", "R", "10", "Q", "10", "9", "J", "Q", "10", "B", "10", "Q", "J", "10", "J", "F", "J", "B", "10", "Q", "J", "B", "Q", "10", "Q", "10", "J"},
	// Reel 2
	{"9", "R", "9", "K", "Q", "B", "K", "J", "F", "B", "9", "K", "9", "B", "W", "K", "9", "J", "K", "W", "Q", "K", "F", "K", "R", "10", "K", "9", "K", "B", "K", "R", "K", "9", "B", "9", "K", "9", "B", "10", "B", "K", "R", "Q", "R", "K", "10", "F", "K", "9", "10", "K", "9", "K", "Q", "K", "R", "9", "K", "9", "K", "F", "R", "9", "K", "10", "Q", "K"},
	// Reel 3
	{"9", "J", "F", "10", "S", "9", "10", "9", "K", "Q", "9", "10", "S", "10", "J", "F", "10", "S", "J", "B", "J", "10", "Q", "R", "J", "R", "Q", "J", "9", "Q", "F", "B", "10", "B", "K", "J", "10", "F", "K", "F", "J", "F", "R", "10", "F", "R", "W", "J", "10", "J", "F", "9", "J", "9", "10", "J", "F", "9", "10"},
	// Reel 4
	{"9", "S", "10", "9", "Q", "J", "S", "Q", "K", "9", "B", "10", "Q", "S", "J", "10", "F", "J", "9", "J", "Q", "J", "R", "Q", "10", "Q", "J", "9", "Q", "J", "9", "Q", "W", "W", "W", "K", "F", "9", "Q", "S", "B", "Q", "10", "J", "K", "R", "10", "B", "J", "Q", "9", "K", "W", "W", "W", "B", "9", "S", "9", "K", "J"},
	// Reel 5
	{"9", "K", "B", "S", "10", "B", "K", "F", "S", "Q", "B", "Q", "J", "K", "W", "F", "Q", "R", "Q", "K", "10", "Q", "F", "9", "F", "B", "K", "B", "S", "10", "J", "K", "B", "10", "J", "B", "R", "B", "S", "F", "K", "F", "B", "F", "K", "10", "B", "Q", "W", "F", "Q", "W", "J", "B", "S", "F", "K", "Q", "K", "F", "S"},
}

func symCode(s string) uint8 {
	switch s {
	case "9":
		return S9
	case "10":
		return S10
	case "J":
		return SJ
	case "Q":
		return SQ
	case "K":
		return SK
	case "R":
		return SR
	case "F":
		return SF
	case "B":
		return SB
	case "W":
		return SW
	case "S":
		return SS
	default:
		panic("unknown symbol: " + s)
	}
}

// 輪帶轉換 字串 → uint8（只在啟動時做一次）
func packReels(src [][]string) [][]uint8 {
	dst := make([][]uint8, len(src))
	for i := range src {
		dst[i] = make([]uint8, len(src[i]))
		for j := range src[i] {
			dst[i][j] = symCode(src[i][j])
		}
	}
	return dst
}

var reelsMG, reelsFG [][]uint8

/**************
 * 轉窗（重用 每把覆寫 各worker各自持有）
 **************/
type window5x3 struct{ c [5][3]uint8 }

func spinWindow(rng *rand.Rand, reels [][]uint8, w *window5x3) {
	for r := 0; r < 5; r++ {
		L := len(reels[r])
		stop := rng.Intn(L)
		w.c[r][0] = reels[r][stop]
		w.c[r][1] = reels[r][(stop+1)%L]
		w.c[r][2] = reels[r][(stop+2)%L]
	}
}

/**************
 * 單符號線評（左到右；W 代、S 斷）
 **************/
func linePay(w *window5x3, line [5]uint8, pay *[NumSymbols][3]float64) float64 {
	target := uint8(255)
	for r := 0; r < 5; r++ {
		s := w.c[r][line[r]]
		if s != SW && s != SS {
			target = s
			break
		}
	}
	/*	if target == 255 {   // For line:SSSSS
				return 0
		}
	*/
	cnt := 0
	for r := 0; r < 5; r++ {
		s := w.c[r][line[r]]
		if s == SS {
			break
		}
		if s == SW || s == target {
			cnt++
		} else {
			break
		}
	}
	if cnt >= 3 {
		return pay[target][cnt-3]
	}
	return 0
}

// 線獎加總
func evalAllLines(w *window5x3, pay *[NumSymbols][3]float64) float64 {
	sum := 0.0
	for i := 0; i < numLines; i++ {
		sum += linePay(w, lines[i], pay)
	}
	return sum
}

func countScatter(w *window5x3) int {
	c := 0
	for r := 0; r < 5; r++ {
		if w.c[r][0] == SS {
			c++
		}
		if w.c[r][1] == SS {
			c++
		}
		if w.c[r][2] == SS {
			c++
		}
	}
	return c
}

func fgMulByScatter(s int) float64 {
	switch {
	case s >= 5:
		return 100
	case s == 4:
		return 50
	case s == 3:
		return 10
	default:
		return 0
	}
}

/**************
 * 一整串 FG
 **************/ //  spins：FG實際跑了幾轉, base：未乘倍率的FG總派彩, retri：再觸發次數, zeroBatches：「全空批次」數, totalBatches：跑了多少個5轉批次
func playFG(rng *rand.Rand, w *window5x3) (spins int, base float64, retri int, zeroBatches int, totalBatches int) {
	queue := 5
	batchSpin := 0
	batchWin := 0.0

	for queue > 0 {
		queue--
		spins++

		spinWindow(rng, reelsFG, w)
		win := evalAllLines(w, &payFG) * betPerLine
		base += win

		if countScatter(w) >= 3 {
			queue += 5
			retri++
		}

		batchSpin++
		batchWin += win
		if batchSpin == 5 {
			totalBatches++
			if batchWin == 0 {
				zeroBatches++
			}
			batchSpin = 0
			batchWin = 0
		}
	}
	if batchSpin > 0 {
		totalBatches++
		if batchWin == 0 {
			zeroBatches++
		}
	}
	return
}

/**************
 * 自訂統計項目
 **************/
type Stats struct {
	mainLineWinSum float64
	freeGameWinSum float64
	triggerCount   int64
	retriggerCount int64
	totalFGSpins   int64
	maxSingleSpin  float64 // 最高單把贏分
	deadSpins      int64   // 空轉數

	trigX10  int64
	trigX50  int64
	trigX100 int64

	bigWins   int64     // ≥20×bet
	megaWins  int64     // ≥60×bet
	superWins int64     // ≥100×bet
	holyWins  int64     // ≥300×bet
	jumboWins int64     // ≥500×bet
	jojoWins  int64     // ≥1000×bet
	hiWinBins [19]int64 // ≥1000×bet 詳細

	fgZeroBatches  int64
	fgTotalBatches int64

	// 用於估計 RTP 的方差與標準誤差
	// 令 x_i = 該把贏分 / 該把總押注（即 per-spin RTP）
	// 這裡做一階與二階統計：sum x_i 與 sum x_i^2
	rtpSum   float64
	rtpSumSq float64
	nSpins   int64
}

var highBinEdges = []float64{
	1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000, 11000, 12000, 13000, 14000, 15000, 20000, 25000, 30000, 40000,
}

/**************
 * 進度心跳
 **************/
var spinsDone int64

func startProgress(total int64) func() {
	start := time.Now()
	tk := time.NewTicker(1 * time.Second) // 每秒回報進度
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-tk.C:
				done := atomic.LoadInt64(&spinsDone)
				elapsed := time.Since(start).Seconds()
				speed := float64(done) / (elapsed + 1e-9)
				eta := float64(total-done) / (speed + 1e-9)
				log.Printf("[PROGRESS] %d/%d (%.2f%%) | %.0f spins/s | ETA %.0fs",
					done, total, 100*float64(done)/float64(total), speed, eta)
			case <-stop:
				tk.Stop()
				return
			}
		}
	}()
	return func() { close(stop) }
}

/**************
 * Worker
 **************/
func worker(_ int, spins int64, out *Stats, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	var w window5x3
	local := Stats{}
	perSpinBet := float64(numLines) * betPerLine

	const bump = 4096 // 設4096轉為一批次
	var bumpCnt int64 = 0

	for i := int64(0); i < spins; i++ {
		// 主遊戲
		spinWindow(rng, reelsMG, &w)
		mgLine := evalAllLines(&w, &payMG) * betPerLine
		spinTotal := mgLine

		// 觸發 FG?
		s := countScatter(&w)
		if s >= 3 {
			local.triggerCount++
			mul := fgMulByScatter(s)

			if s >= 5 {
				local.trigX100++
			} else if s == 4 {
				local.trigX50++
			} else {
				local.trigX10++
			}

			fgSp, fgBase, retri, zeroB, totalB := playFG(rng, &w)
			local.totalFGSpins += int64(fgSp)
			local.retriggerCount += int64(retri)
			local.fgZeroBatches += int64(zeroB)
			local.fgTotalBatches += int64(totalB)

			fgWin := fgBase * mul
			local.freeGameWinSum += fgWin
			spinTotal += fgWin
		} else if mgLine == 0 {
			local.deadSpins++
		}

		local.mainLineWinSum += mgLine
		if spinTotal > local.maxSingleSpin {
			local.maxSingleSpin = spinTotal
		}

		// 互斥分層（以單把贏分/押注）
		ratio := spinTotal / perSpinBet
		if ratio >= 1000 {
			local.jojoWins++
		} else if ratio >= 500 {
			local.jumboWins++
		} else if ratio >= 300 {
			local.holyWins++
		} else if ratio >= 100 {
			local.superWins++
		} else if ratio >= 60 {
			local.megaWins++
		} else if ratio >= 20 {
			local.bigWins++
		}

		//x1000 以上更細的互斥分層
		if ratio >= highBinEdges[0] {
			// 從最大門檻往回找，找到第一個符合 ratio >= edge 的 bin
			for i := len(highBinEdges) - 1; i >= 0; i-- {
				if ratio >= highBinEdges[i] {
					local.hiWinBins[i]++
					break
				}
			}
		}

		// per-spin RTP 的一階、二階統計
		local.rtpSum += ratio
		local.rtpSumSq += ratio * ratio
		local.nSpins++

		// 4096轉為一批次累加 (降低 atomic 次數）
		bumpCnt++
		if bumpCnt == bump {
			atomic.AddInt64(&spinsDone, bump)
			bumpCnt = 0
		}
	}
	if bumpCnt > 0 { // 最後不足一批次的累加
		atomic.AddInt64(&spinsDone, bumpCnt)
	}

	*out = local
}

/**************
 * 輔助輸出：「約每 N 轉一次」
 **************/
func everyStr(totalSpins, count int64) string {
	if count <= 0 {
		return "（—）"
	}
	return fmt.Sprintf("（約每 %.0f 轉一次）", float64(totalSpins)/float64(count))
}

/**************
 * 主程式
 **************/
func main() {

	// 啟動：載入輪帶
	reelsMG = packReels(reelsMGstr)
	reelsFG = packReels(reelsFGstr)

	// 進度心跳
	runtime.GOMAXPROCS(numWorkers)
	stopHb := startProgress(numSpins)
	defer stopHb()

	totalBet := float64(numSpins) * float64(numLines) * betPerLine
	perSpinBet := float64(numLines) * betPerLine

	// 並行 : 每個worker皆把各自轉數跑完(即總轉數完成)才做輸出
	var wg sync.WaitGroup
	wg.Add(numWorkers)                 // 會有 numWorkers 個工作要等
	stats := make([]Stats, numWorkers) // 建一個長度為 numWorkers 的切片,讓各worker把統計結果寫進各自的（&stats[i]）
	chunk := numSpins / int64(numWorkers)
	rem := numSpins % int64(numWorkers)

	baseSeed := time.Now().UnixNano()
	for w := 0; w < numWorkers; w++ { // 攤分總spin
		spins := chunk
		if int64(w) < rem {
			spins++
		}
		go func(i int, n int64) {
			defer wg.Done()
			worker(i, n, &stats[i], baseSeed+int64(i)*1337) // i:worker ID,n:該worker應跑轉數,各worker的RNG種子不同，避免序列重疊
		}(w, spins)
	}
	wg.Wait()

	// 匯總
	total := Stats{}
	for i := 0; i < numWorkers; i++ {
		s := stats[i]
		total.mainLineWinSum += s.mainLineWinSum
		total.freeGameWinSum += s.freeGameWinSum
		total.triggerCount += s.triggerCount
		total.retriggerCount += s.retriggerCount
		total.totalFGSpins += s.totalFGSpins
		if s.maxSingleSpin > total.maxSingleSpin {
			total.maxSingleSpin = s.maxSingleSpin
		}
		total.deadSpins += s.deadSpins
		total.trigX10 += s.trigX10
		total.trigX50 += s.trigX50
		total.trigX100 += s.trigX100

		total.bigWins += s.bigWins
		total.megaWins += s.megaWins
		total.superWins += s.superWins
		total.holyWins += s.holyWins
		total.jumboWins += s.jumboWins
		total.jojoWins += s.jojoWins

		for i := range total.hiWinBins {
			total.hiWinBins[i] += s.hiWinBins[i]
		}

		total.fgZeroBatches += s.fgZeroBatches
		total.fgTotalBatches += s.fgTotalBatches

		// per-spin RTP 一階/二階統計
		total.rtpSum += s.rtpSum
		total.rtpSumSq += s.rtpSumSq
		total.nSpins += s.nSpins
	}

	// 指標
	totalWin := total.mainLineWinSum + total.freeGameWinSum
	rtpMG := total.mainLineWinSum / totalBet
	rtpFG := total.freeGameWinSum / totalBet
	rtpTotal := totalWin / totalBet

	fmt.Printf("=== Monte Carlo | workers=%d | spins=%d | lines=%d | bet/line=%.2f ===\n",
		numWorkers, numSpins, numLines, betPerLine)
	fmt.Printf("總成本 (Total Bet)                    : %.2f\n", totalBet)
	fmt.Printf("總贏分 (Total Win)                    : %.2f\n", totalWin)
	fmt.Printf("最高單把贏分                           : %.2f (x%.2f)\n", total.maxSingleSpin, total.maxSingleSpin/perSpinBet)
	fmt.Printf("主遊戲 RTP                            : %.6f\n", rtpMG)
	fmt.Printf("免費遊戲 RTP                          : %.6f\n", rtpFG)
	fmt.Printf("總 RTP                                : %.6f\n", rtpTotal)

	// 觸發與再觸發
	fmt.Printf("免費遊戲觸發次數                       : %d (觸發率 %.6f) %s\n",
		total.triggerCount, float64(total.triggerCount)/float64(numSpins), everyStr(numSpins, total.triggerCount))
	fmt.Printf("  └×10  次數 (3S)                     : %d %s\n", total.trigX10, everyStr(numSpins, total.trigX10))
	fmt.Printf("  └×50  次數 (4S)                     : %d %s\n", total.trigX50, everyStr(numSpins, total.trigX50))
	fmt.Printf("  └×100 次數 (5S)                     : %d %s\n", total.trigX100, everyStr(numSpins, total.trigX100))

	retriRate := 0.0
	if total.triggerCount > 0 {
		retriRate = float64(total.retriggerCount) / float64(total.totalFGSpins)
	}
	fmt.Printf("免費遊戲再觸發次數                     : %d（再觸發率 %.6f）\n", total.retriggerCount, retriRate)

	if total.triggerCount > 0 {
		fmt.Printf("每次免費遊戲平均場次                   : %.3f\n", float64(total.totalFGSpins)/float64(total.triggerCount))
	}

	// FG 5 轉批次全空
	if total.fgTotalBatches > 0 {
		fmt.Printf("FG 5轉批次完全無贏分                   : %d / %d (占比 %.6f)\n",
			total.fgZeroBatches, total.fgTotalBatches,
			float64(total.fgZeroBatches)/float64(total.fgTotalBatches))
	} else {
		fmt.Printf("FG 5轉批次完全無贏分                   : 0 / 0 (占比 0)\n")
	}

	fmt.Printf("主遊戲 dead spins（無線獎且未觸發FG）: %d (占比 %.6f)\n", total.deadSpins, float64(total.deadSpins)/float64(numSpins))

	fmt.Printf("\n獎項分佈\n")
	fmt.Printf("Big  Win  (≥20×bet)                   : %d %s\n", total.bigWins, everyStr(numSpins, total.bigWins))
	fmt.Printf("Mega Win  (≥60×bet)                   : %d %s\n", total.megaWins, everyStr(numSpins, total.megaWins))
	fmt.Printf("Super Win (≥100×bet)                  : %d %s\n", total.superWins, everyStr(numSpins, total.superWins))
	fmt.Printf("Holy Win (≥300×bet)                   : %d %s\n", total.holyWins, everyStr(numSpins, total.holyWins))
	fmt.Printf("Jumbo Win (≥500×bet)                  : %d %s\n", total.jumboWins, everyStr(numSpins, total.jumboWins))
	fmt.Printf("Jojo Win  (≥1000×bet)                 : %d %s\n", total.jojoWins, everyStr(numSpins, total.jojoWins))

	fmt.Printf("\n≥1000倍大獎項細分（互斥）\n")
	for i, edge := range highBinEdges {
		cnt := total.hiWinBins[i]
		fmt.Printf("≥%-5d×bet    : %d %s\n",
			int(edge), cnt, everyStr(numSpins, cnt))
	}

	// 統計推論（以 per-spin RTP 為隨機變數）
	n := float64(total.nSpins)
	mean := total.rtpSum / n
	meanSq := total.rtpSumSq / n
	variance := meanSq - mean*mean
	if variance < 0 {
		variance = 0 // 浮點保險
	}
	se := math.Sqrt(variance / n)
	lo := mean - 1.96*se
	hi := mean + 1.96*se

	fmt.Printf("\n=== 統計推論（per-spin RTP） ===\n")
	fmt.Printf("樣本數 n                              : %.0f\n", n)
	fmt.Printf("樣本均值 mean(RTP)                    : %.6f\n", mean)
	fmt.Printf("樣本方差 var(RTP)                     : %.6f\n", variance)
	fmt.Printf("標準誤差 SE                           : %.6f\n", se)
	fmt.Printf("95%% 信賴區間                          : [%.6f, %.6f]\n", lo, hi)

	if excelRTP >= 0 {
		z := (excelRTP - mean) / se
		inCI := (excelRTP >= lo && excelRTP <= hi)
		fmt.Printf("Excel RTP                             : %.6f\n", excelRTP)
		fmt.Printf("Excel 與樣本均值差的 z 分數            : %.2f\n", z)
		if inCI {
			fmt.Printf("結論：Excel 值落在本次 95%% CI 之內（可視為誤差內）。\n")
		} else {
			fmt.Printf("結論：Excel 值不在本次 95%% CI 之內（建議檢查）。\n")
		}
	}
}
