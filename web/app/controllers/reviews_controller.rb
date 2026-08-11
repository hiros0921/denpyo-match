# 承認キュー。事務所が一番長く見る画面。
#
# 要確認と却下を分けて出す。同じ「人が見る」でも意味が違う。
#   要確認  候補はある。合っているか確かめてほしい
#   却下    候補が無い／低すぎる。画像を見て手で入れるしかない
class ReviewsController < ApplicationController
  def index
    base = MatchResult.joins(document: :client)
                      .where(clients: { organization_id: current_organization.id })
    base = base.where(documents: { client_id: params[:client_id] }) if params[:client_id].present?

    @needs_review = load(base.where(decision: MatchResult::NEEDS_REVIEW))
    @rejected     = load(base.where(decision: MatchResult::REJECT))
    @clients = accessible_clients
  end

  private

  def load(scope)
    scope.includes(document: [:client, :extracted_fields], partner: {})
         .order("match_results.score DESC NULLS LAST").limit(50)
  end
end
