/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package invoke

import (
	"bytes"

	"github.com/hyperledger/fabric-sdk-go/pkg/common/options"
	"github.com/hyperledger/fabric-sdk-go/pkg/util/errors/status"
	"github.com/pkg/errors"

	selectopts "github.com/hyperledger/fabric-sdk-go/pkg/client/common/selection/options"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/logging"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/fab"
	"github.com/hyperledger/fabric-sdk-go/pkg/fab/peer"
	"github.com/hyperledger/fabric-sdk-go/pkg/fab/txn"
	"github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/peer"
)

var logger = logging.NewLogger("fabsdk/client")

//EndorsementHandler for handling endorse transactions
type EndorsementHandler struct {
	next Handler
}

//Handle for endorsing transactions
func (e *EndorsementHandler) Handle(requestContext *RequestContext, clientContext *ClientContext) {

	if len(requestContext.Opts.Targets) == 0 {
		requestContext.Error = status.New(status.ClientStatus, status.NoPeersFound.ToInt32(), "targets were not provided", nil)
		return
	}

	// Endorse Tx
	transactionProposalResponses, proposal, err := createAndSendTransactionProposal(clientContext.Transactor, &requestContext.Request, peer.PeersToTxnProcessors(requestContext.Opts.Targets))

	requestContext.Response.Proposal = proposal
	requestContext.Response.TransactionID = proposal.TxnID // TODO: still needed?

	if err != nil {
		requestContext.Error = err
		return
	}

	requestContext.Response.Responses = transactionProposalResponses
	if len(transactionProposalResponses) > 0 {
		requestContext.Response.Payload = transactionProposalResponses[0].ProposalResponse.GetResponse().Payload
	}

	//Delegate to next step if any
	if e.next != nil {
		e.next.Handle(requestContext, clientContext)
	}
}

//ProposalProcessorHandler for selecting proposal processors
type ProposalProcessorHandler struct {
	next Handler
}

//Handle selects proposal processors
func (h *ProposalProcessorHandler) Handle(requestContext *RequestContext, clientContext *ClientContext) {
	//Get proposal processor, if not supplied then use selection service to get available peers as endorser
	if len(requestContext.Opts.Targets) == 0 {
		var selectionOpts []options.Opt
		if requestContext.SelectionFilter != nil {
			selectionOpts = append(selectionOpts, selectopts.WithPeerFilter(requestContext.SelectionFilter))
		}
		endorsers, err := clientContext.Selection.GetEndorsersForChaincode([]string{requestContext.Request.ChaincodeID}, selectionOpts...)
		if err != nil {
			requestContext.Error = errors.WithMessage(err, "Failed to get endorsing peers")
			return
		}
		requestContext.Opts.Targets = endorsers
	}

	//Delegate to next step if any
	if h.next != nil {
		h.next.Handle(requestContext, clientContext)
	}
}

//EndorsementValidationHandler for transaction proposal response filtering
type EndorsementValidationHandler struct {
	next Handler
}

//Handle for Filtering proposal response
func (f *EndorsementValidationHandler) Handle(requestContext *RequestContext, clientContext *ClientContext) {

	//Filter tx proposal responses
	err := f.validate(requestContext.Response.Responses)
	if err != nil {
		requestContext.Error = errors.WithMessage(err, "endorsement validation failed")
		return
	}

	//Delegate to next step if any
	if f.next != nil {
		f.next.Handle(requestContext, clientContext)
	}
}

func (f *EndorsementValidationHandler) validate(txProposalResponse []*fab.TransactionProposalResponse) error {
	var a1 []byte
	for n, r := range txProposalResponse {
		if r.ProposalResponse.GetResponse().Status != int32(common.Status_SUCCESS) {
			return status.NewFromProposalResponse(r.ProposalResponse, r.Endorser)
		}
		if n == 0 {
			a1 = r.ProposalResponse.GetResponse().Payload
			continue
		}

		if bytes.Compare(a1, r.ProposalResponse.GetResponse().Payload) != 0 {
			return status.New(status.EndorserClientStatus, status.EndorsementMismatch.ToInt32(),
				"ProposalResponsePayloads do not match", nil)
		}
	}

	return nil
}

//CommitTxHandler for committing transactions
type CommitTxHandler struct {
	next Handler
}

//Handle handles commit tx
func (c *CommitTxHandler) Handle(requestContext *RequestContext, clientContext *ClientContext) {
	txnID := requestContext.Response.TransactionID

	//Register Tx event
	reg, statusNotifier, err := clientContext.EventService.RegisterTxStatusEvent(string(txnID)) // TODO: Change func to use TransactionID instead of string
	if err != nil {
		requestContext.Error = errors.Wrap(err, "error registering for TxStatus event")
		return
	}
	defer clientContext.EventService.Unregister(reg)

	_, err = createAndSendTransaction(clientContext.Transactor, requestContext.Response.Proposal, requestContext.Response.Responses)
	if err != nil {
		requestContext.Error = errors.Wrap(err, "CreateAndSendTransaction failed")
		return
	}

	select {
	case txStatus := <-statusNotifier:
		requestContext.Response.TxValidationCode = txStatus.TxValidationCode

		if txStatus.TxValidationCode != pb.TxValidationCode_VALID {
			requestContext.Error = status.New(status.EventServerStatus, int32(txStatus.TxValidationCode), "received invalid transaction", nil)
			return
		}
	case <-requestContext.Ctx.Done():
		requestContext.Error = errors.New("Execute didn't receive block event")
		return
	}

	//Delegate to next step if any
	if c.next != nil {
		c.next.Handle(requestContext, clientContext)
	}
}

//NewQueryHandler returns query handler with EndorseTxHandler & EndorsementValidationHandler Chained
func NewQueryHandler(next ...Handler) Handler {
	return NewProposalProcessorHandler(
		NewEndorsementHandler(
			NewEndorsementValidationHandler(
				NewSignatureValidationHandler(next...),
			),
		),
	)
}

//NewExecuteHandler returns query handler with EndorseTxHandler, EndorsementValidationHandler & CommitTxHandler Chained
func NewExecuteHandler(next ...Handler) Handler {
	return NewProposalProcessorHandler(
		NewEndorsementHandler(
			NewEndorsementValidationHandler(
				NewSignatureValidationHandler(NewCommitHandler(next...)),
			),
		),
	)
}

//NewProposalProcessorHandler returns a handler that selects proposal processors
func NewProposalProcessorHandler(next ...Handler) *ProposalProcessorHandler {
	return &ProposalProcessorHandler{next: getNext(next)}
}

//NewEndorsementHandler returns a handler that endorses a transaction proposal
func NewEndorsementHandler(next ...Handler) *EndorsementHandler {
	return &EndorsementHandler{next: getNext(next)}
}

//NewEndorsementValidationHandler returns a handler that validates an endorsement
func NewEndorsementValidationHandler(next ...Handler) *EndorsementValidationHandler {
	return &EndorsementValidationHandler{next: getNext(next)}
}

//NewCommitHandler returns a handler that commits transaction propsal responses
func NewCommitHandler(next ...Handler) *CommitTxHandler {
	return &CommitTxHandler{next: getNext(next)}
}

func getNext(next []Handler) Handler {
	if len(next) > 0 {
		return next[0]
	}
	return nil
}

func createAndSendTransaction(sender fab.Sender, proposal *fab.TransactionProposal, resps []*fab.TransactionProposalResponse) (*fab.TransactionResponse, error) {

	txnRequest := fab.TransactionRequest{
		Proposal:          proposal,
		ProposalResponses: resps,
	}

	tx, err := sender.CreateTransaction(txnRequest)
	if err != nil {
		return nil, errors.WithMessage(err, "CreateTransaction failed")
	}

	transactionResponse, err := sender.SendTransaction(tx)
	if err != nil {
		return nil, errors.WithMessage(err, "SendTransaction failed")

	}

	return transactionResponse, nil
}

func createAndSendTransactionProposal(transactor fab.Transactor, chrequest *Request, targets []fab.ProposalProcessor) ([]*fab.TransactionProposalResponse, *fab.TransactionProposal, error) {
	request := fab.ChaincodeInvokeRequest{
		ChaincodeID:  chrequest.ChaincodeID,
		Fcn:          chrequest.Fcn,
		Args:         chrequest.Args,
		TransientMap: chrequest.TransientMap,
	}

	txh, err := transactor.CreateTransactionHeader()
	if err != nil {
		return nil, nil, errors.WithMessage(err, "creating transaction header failed")
	}

	proposal, err := txn.CreateChaincodeInvokeProposal(txh, request)
	if err != nil {
		return nil, nil, errors.WithMessage(err, "creating transaction proposal failed")
	}

	transactionProposalResponses, err := transactor.SendTransactionProposal(proposal, targets)
	return transactionProposalResponses, proposal, err
}
