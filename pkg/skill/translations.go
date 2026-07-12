package skill

// englishSkillTranslations is keyed by the stable canonical Skill ID rather
// than mutable database copy. Custom Skills intentionally have no implicit
// translation; clients can apply a locale-safe ID fallback without exposing
// copy written for another locale.
var englishSkillTranslations = map[string]SkillTranslation{
	"content/translation": {
		Name:        "Translation",
		Description: "Translate between Chinese, English, and other languages with terminology-aware localization.",
	},
	"content/summarization": {
		Name:        "Summarization",
		Description: "Condense long text, extract key points, and produce meeting notes.",
	},
	"content/copywriting": {
		Name:        "Copywriting",
		Description: "Create marketing copy, social posts, product descriptions, and advertising concepts.",
	},
	"content/proofreading": {
		Name:        "Proofreading and Editing",
		Description: "Correct spelling and grammar, align writing style, and improve content for search.",
	},
	"content/structured-data": {
		Name:        "Structured Extraction",
		Description: "Extract people, places, dates, amounts, and fields from unstructured text.",
	},
	"dev/code-review": {
		Name:        "Code Review",
		Description: "Review pull requests for style issues, risks, and potential bugs.",
	},
	"dev/code-generation": {
		Name:        "Code Generation",
		Description: "Generate functions, tests, and project scaffolding from a specification.",
	},
	"dev/code-explanation": {
		Name:        "Code Explanation",
		Description: "Explain legacy code, write documentation, and map sequence flows.",
	},
	"dev/test-generation": {
		Name:        "Test Generation",
		Description: "Generate unit tests, integration tests, and mock data.",
	},
	"dev/devops-ci": {
		Name:        "CI/CD and DevOps",
		Description: "Build GitHub Actions workflows, delivery pipelines, and deployment scripts.",
	},
	"data/sql-query": {
		Name:        "SQL Query",
		Description: "Translate natural language into SQL, optimize slow queries, and explain schemas.",
	},
	"data/data-cleaning": {
		Name:        "Data Cleaning",
		Description: "Deduplicate data, fill missing values, convert types, and detect anomalies.",
	},
	"data/analysis": {
		Name:        "Data Analysis",
		Description: "Analyze statistics and trends, compare periods, and summarize insights.",
	},
	"data/visualization": {
		Name:        "Data Visualization",
		Description: "Generate chart configurations, dashboard specifications, and Mermaid diagrams.",
	},
	"data/forecasting": {
		Name:        "Forecasting",
		Description: "Forecast time series, sales, traffic, and inventory.",
	},
	"media/image-generate": {
		Name:        "Image Generation",
		Description: "Create images, product visuals, posters, and avatars from prompts.",
	},
	"media/image-edit": {
		Name:        "Image Editing",
		Description: "Remove backgrounds, retouch, restyle, and upscale images.",
	},
	"media/audio-transcribe": {
		Name:        "Audio Transcription",
		Description: "Transcribe multilingual audio, generate subtitles, and separate speakers.",
	},
	"media/audio-generate": {
		Name:        "Speech Generation",
		Description: "Synthesize multi-speaker speech with voice, tone, and emotion controls.",
	},
	"media/video-process": {
		Name:        "Video Processing",
		Description: "Edit video, add subtitles, create clips, and summarize footage.",
	},
	"ops/document-generate": {
		Name:        "Document Generation",
		Description: "Create PDFs, Word documents, reports, contracts, and resumes.",
	},
	"ops/email-process": {
		Name:        "Email Processing",
		Description: "Classify email, draft replies, extract content, and support routing decisions.",
	},
	"ops/scheduling": {
		Name:        "Scheduling",
		Description: "Coordinate meetings, reminders, and time zones.",
	},
	"ops/web-scraping": {
		Name:        "Web Scraping",
		Description: "Collect data from websites and APIs for monitoring and price tracking.",
	},
	"ops/notification": {
		Name:        "Notifications",
		Description: "Deliver notifications through WeChat, Slack, email, and SMS.",
	},
	"ai/rag": {
		Name:        "RAG Retrieval",
		Description: "Answer questions over knowledge bases with semantic and document retrieval.",
	},
	"ai/agent-orchestration": {
		Name:        "Agent Orchestration",
		Description: "Coordinate multi-step Agent collaboration and tool workflows.",
	},
	"ai/finetune": {
		Name:        "Model Fine-tuning",
		Description: "Prepare data and run LoRA, SFT, and evaluation workflows.",
	},
	"ai/prompt-engineering": {
		Name:        "Prompt Engineering",
		Description: "Iterate prompts, run A/B tests, and design few-shot examples.",
	},
	"ai/safety-eval": {
		Name:        "Safety Evaluation",
		Description: "Evaluate hallucinations, jailbreak resistance, and compliance risks.",
	},
}

func translationsForSkill(skillID string) map[string]SkillTranslation {
	translation, ok := englishSkillTranslations[skillID]
	if !ok {
		return nil
	}
	return map[string]SkillTranslation{"en": translation}
}
