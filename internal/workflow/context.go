package workflow

type Context interface {
	Replaying() bool

	RegisterResult()
}

func NewContext() Context {
	return &contextImpl{}
}

type contextImpl struct {
}

func (c *contextImpl) Replaying() bool {
	return false // TODO
}

func (c *contextImpl) RegisterResult() {
	panic("not implemented")
}
