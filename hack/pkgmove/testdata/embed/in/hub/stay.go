package hub

func Use(o *Outer) int { return o.inner.Value + o.Value }
