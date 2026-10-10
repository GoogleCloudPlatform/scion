package hub

func Use(o *Outer) int { return o.Inner.Value + o.Value }
