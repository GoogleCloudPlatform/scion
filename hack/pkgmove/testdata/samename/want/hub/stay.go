package hub

// summary is rendered with text/template.
const summary = "{{.Count}} things"

// Total uses every count method.
func Total() int {
	return Alpha{}.Count() + Beta{}.Count() + Gamma{}.Count() + Delta{}.Count()
}
