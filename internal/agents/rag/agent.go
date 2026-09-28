package rag

import (
	"context"
	"fmt"
	"sort"
	"sync"
	vatel "va_adk_pilot/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"va_adk_pilot/internal/config"
	"va_adk_pilot/internal/embed"
	"va_adk_pilot/internal/loopguard"
)

// Corpus is the in-memory knowledge base (the PRD allows a mock vector DB;
// the vectors themselves are real embeddings computed at first use).
var Corpus = map[string]string{
	"doc1":  "ต้องใช้บัตรประจำตัวประชาชนตัวจริงค่ะ สำหรับการเปิดบัญชีที่สาขา",
	"doc2":  "การเปิดบัญชีออนไลน์สามารถทำได้ผ่านแอปพลิเคชันโดยใช้การยืนยันตัวตน NDID",
	"doc3":  "บัญชีเงินฝากออมทรัพย์ดอกเบี้ย 0.5% ต่อปี เปิดใหม่ต้องใช้เงินฝากขั้นต่ำ 500 บาท",
	"doc4":  "การโอนเงินสามารถทำได้ผ่านแอป หรือหน้าตู้",
	"doc5":  "โอนเงินต่างธนาคารจะมีค่าธรรมเนียมตามที่กำหนด หรือฟรีหากผ่านพร้อมเพย์",
	"doc6":  "สินเชื่อบ้านมีดอกเบี้ยพิเศษสำหรับลูกค้ารายใหม่",
	"doc7":  "ต้องใช้สลิปเงินเดือนและสมุดบัญชีธนาคารเพื่อยื่นขอสินเชื่อบ้านค่ะ",
	"doc8":  "สมัครบัตรเครดิตต้องมีรายได้ขั้นต่ำ 15,000 บาทต่อเดือน และใช้สเตทเมนต์ย้อนหลัง 6 เดือน",
	"doc9":  "บัตรเครดิตสามารถกดเงินสดได้ที่ตู้ ATM แต่จะมีค่าธรรมเนียมเบิกถอนเงินสดล่วงหน้า 3%",
	"doc10": "การขอยกเลิกบัตรเครดิต สามารถโทรติดต่อ Call Center ได้ตลอด 24 ชั่วโมง",
	"doc11": "เปิดบัญชีเพื่อธุรกิจต้องใช้หนังสือรับรองจดทะเบียนบริษัทและบัตรประชาชนกรรมการ",
	"doc12": "แอปพลิเคชันเวอร์ชันใหม่ รองรับการทำธุรกรรมจากบัตรประชาชนออนไลน์ที่ผ่านกรมการปกครอง",
	"doc13": "ลืมรหัสผ่านแอป สามารถกดลืมรหัสและยืนยันด้วยใบหน้าได้เลยนะคะ",
	"doc14": "ตู้ ATM ของธนาคารเราสามารถฝากเงินสดโดยใช้บัตรประชาชนยืนยันตัวตนได้แล้ววันนี้",
	"doc15": "สมุดบัญชีหาย ต้องแจ้งความก่อนนำใบแจ้งความมาขอทำสมุดบัญชีใหม่ที่สาขานะคะ",
	"doc16": "บัตร ATM หาย สามารถอายัดผ่านแอปได้ทันที เพื่อความปลอดภัยค่ะ",
	"doc17": "อายัดบัตรเครดิต โปรดติดต่อคอลเซ็นเตอร์ หรือระงับชั่วคราวผ่านแอป",
	"doc18": "การขอ Statement ย้อนหลัง 1 ปี สามารถทำผ่านแอปได้ฟรี ไม่ต้องมาสาขา",
	"doc19": "บัญชีฝากประจำ 12 เดือน ให้ดอกเบี้ยสูงสุด 2.0% มีขั้นต่ำการฝาก 1,000 บาท",
	"doc20": "บัญชีฝากประจำปลอดภาษี 24 เดือน สร้างวินัยการออมด้วยการฝากเท่ากันทุกเดือน",
	"doc21": "สลากออมทรัพย์ ซื้อได้ขั้นต่ำ 100 บาท ลุ้นรางวัลใหญ่ทุกเดือน",
	"doc22": "กองทุนรวม สามารถเปิดบัญชีและซื้อขายผ่านแอปพลิเคชันได้โดยตรง",
	"doc23": "สินเชื่อส่วนบุคคลไม่ต้องใช้หลักทรัพย์ค้ำประกัน อนุมัติไวภายใน 1 วัน",
	"doc24": "เปิดบัญชีร่วม ต้องมาที่สาขาทั้งสองท่าน พร้อมบัตรประชาชนตัวจริงนะคะ",
	"doc25": "สินเชื่อรถยนต์ สามารถรีไฟแนนซ์เพื่อรวมหนี้และลดดอกเบี้ยได้",
	"doc26": "ประกันอุบัติเหตุ PA คุ้มครองครอบคลุมถึงค่ารักษาพยาบาล ไม่ต้องสำรองจ่าย",
	"doc27": "บัตรเดบิตมีค่าธรรมเนียมรายปี 200 บาท หักจากบัญชีอัตโนมัติ",
	"doc28": "ค่าธรรมเนียมรักษาบัญชีคือ 50 บาท/เดือน หากไม่เคลื่อนไหวเกิน 1 ปีและเงินต่ำกว่า 2,000 บาท",
	"doc29": "การโอนเงินไปต่างประเทศผ่านแอป มีค่าธรรมเนียมเพียง 150 บาทต่อรายการ",
	"doc30": "เช็คธนาคารและแคชเชียร์เช็ค สามารถติดต่อขอซื้อได้ที่สาขาธนาคารทุกแห่ง",
	"doc31": "ขอสินเชื่อธุรกิจ SME ใช้ระยะเวลาพิจารณาประมาณ 7-14 วันทำการ",
	"doc32": "ชำระบิลผ่านแอปทำได้มากกว่า 1,000 รายการ ฟรีค่าธรรมเนียม",
	"doc33": "บริการพร้อมเพย์ เชื่อมโยงบัญชีกับเบอร์โทรศัพท์หรือเลขบัตรประชาชน",
	"doc34": "ลูกค้าใหม่ดาวน์โหลดแอปและสมัครใช้งานรับส่วนลดพิเศษมากมาย",
	"doc35": "ตู้ฝากเงินหน้าสาขารองรับเฉพาะธนบัตร 100, 500, 1000 บาทเท่านั้น",
	"doc36": "อัตราแลกเปลี่ยนสกุลเงินต่างประเทศ อัปเดตทุกวันบนแอปและหน้าเว็บไซต์ของเราค่ะ",
	"doc37": "หนังสือรับรองฐานะการเงิน สามารถขอได้ที่สาขาหรือผ่านระบบออนไลน์",
}

// Retriever is an in-memory vector index over Corpus.
type Retriever struct {
	Embedder embed.Embedder
	TopK     int

	once sync.Once
	err  error
	ids  []string
	vecs [][]float64
}

func NewRetriever(e embed.Embedder, topK int) *Retriever {
	return &Retriever{Embedder: embed.Traced(e), TopK: topK}
}

func (r *Retriever) build(ctx context.Context) error {
	r.once.Do(func() {
		ids := make([]string, 0, len(Corpus))
		for id := range Corpus {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		texts := make([]string, len(ids))
		for i, id := range ids {
			texts[i] = Corpus[id]
		}
		vecs, err := r.Embedder.Embed(ctx, texts, "document")
		if err != nil {
			r.err = fmt.Errorf("building vector index: %w", err)
			r.once = sync.Once{} // allow retry on next call
			return
		}
		r.ids, r.vecs = ids, vecs
	})
	return r.err
}

type RetrievedDoc struct {
	ID      string  `json:"id"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// Search embeds the query and returns the top-k documents by cosine similarity.
func (r *Retriever) Search(ctx context.Context, query string) ([]RetrievedDoc, error) {
	if err := r.build(ctx); err != nil {
		return nil, err
	}
	qv, err := r.Embedder.Embed(ctx, []string{query}, "query")
	if err != nil {
		return nil, err
	}
	docs := make([]RetrievedDoc, len(r.ids))
	for i, id := range r.ids {
		docs[i] = RetrievedDoc{ID: id, Content: Corpus[id], Score: embed.Cosine(qv[0], r.vecs[i])}
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].Score > docs[j].Score })
	if len(docs) > r.TopK {
		docs = docs[:r.TopK]
	}
	return docs, nil
}

type SearchArgs struct {
	Query string `json:"query"`
}

type SearchResults struct {
	Results []RetrievedDoc `json:"results"`
}

const Instruction = `You are the Krungthai Bank virtual assistant for general banking questions. Reply in Thai.
Always call SearchBankingDocs first, then answer ONLY from the returned documents and cite their ids like (doc3).
If the documents do not answer the question, say you don't have that information.
The customer's personal data may appear as placeholders such as [EMAIL_1], [PHONE_1] or [ACCOUNT_1]; keep any placeholder you need exactly as written and never invent personal data.`

// NewTools returns the RAG agent's tools bound to a retriever.
func NewTools(r *Retriever) ([]tool.Tool, error) {
	t, err := functiontool.New(functiontool.Config{
		Name:        "SearchBankingDocs",
		Description: "Semantic search over Krungthai banking policy documents. Returns the top matching documents with similarity scores.",
	}, func(ctx agent.Context, args SearchArgs) (SearchResults, error) {
		docs, err := r.Search(ctx, args.Query)
		if err != nil {
			return SearchResults{}, err
		}
		ids := make([]string, len(docs))
		for i, d := range docs {
			ids[i] = d.ID
		}
		ctx.State().Set("rag.last_query", args.Query)
		ctx.State().Set("rag.last_retrieved_ids", ids)
		vatel.SetAttrs(ctx, "va.attrs",
			attribute.String("va.rag.query", args.Query),
			attribute.StringSlice("va.rag.retrieved_ids", ids),
		)
		return SearchResults{Results: docs}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.Tool{t}, nil
}

var (
	defaultOnce sync.Once
	defaultRet  *Retriever
)

// DefaultRetriever is shared by all agent instances in the process.
func DefaultRetriever() *Retriever {
	defaultOnce.Do(func() {
		defaultRet = NewRetriever(embed.FromEnv(config.GetGatewayURL()), 3)
	})
	return defaultRet
}

func NewAgent(mdl model.LLM) (agent.Agent, error) {
	tools, err := NewTools(DefaultRetriever())
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:                 "rag",
		Description:          "Answers banking FAQs (accounts, cards, fees, loans info) from the policy knowledge base.",
		Instruction:          Instruction,
		Model:                mdl,
		Tools:                tools,
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{loopguard.New(loopguard.MaxFromEnv())},
	})
}
