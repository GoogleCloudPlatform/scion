package hub

// summary is rendered with text/template.
const summary = "{{.Count}} things"

// Total uses every count method.
func Total() int {
	return Alpha{}.count() + Beta{}.count() + Gamma{}.count() + Delta{}.count()
}
