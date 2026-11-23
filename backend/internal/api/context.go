package api

import "context"

func withRouteSlot(ctx context.Context, s *routeSlot) context.Context {
	return context.WithValue(ctx, routeKey{}, s)
}

func routeSlotFrom(ctx context.Context) *routeSlot {
	s, _ := ctx.Value(routeKey{}).(*routeSlot)
	return s
}
