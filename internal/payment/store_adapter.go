package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aliaankhowaja/sockt-backend/internal/store"
	"github.com/google/uuid"
)

// StoreAgentOffers loads agent offer metadata from Supabase.
type StoreAgentOffers struct {
	St *store.Store
}

// Lookup implements AgentOffers.
func (s *StoreAgentOffers) Lookup(ctx context.Context, agentID uuid.UUID) (AgentOfferInfo, error) {
	if s == nil || s.St == nil {
		return AgentOfferInfo{}, classifyErr(ClassOfferInvalid, errors.New("nil store"))
	}
	a, err := s.St.GetAgentByID(ctx, agentID)
	if err != nil {
		return AgentOfferInfo{}, err
	}
	if a == nil {
		return AgentOfferInfo{}, classifyErr(ClassOfferInvalid, fmt.Errorf("agent %s not found", agentID))
	}
	info := AgentOfferInfo{OfferType: strings.TrimSpace(a.OfferType)}
	if a.Bolt12Offer != nil {
		info.Bolt12Offer = strings.TrimSpace(*a.Bolt12Offer)
	}
	if a.LightningAddress != nil {
		info.LightningAddress = strings.TrimSpace(*a.LightningAddress)
	}
	return info, nil
}
