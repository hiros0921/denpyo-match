# 誤認識の影響範囲。
#
# 誤りが1件見つかったとき、「同じ誤りが他にも及んでいないか」を調べる画面。
# 会計事務所は顧問先の帳簿を預かっているので、1件の誤りが分かった時点で
# 「では他は大丈夫なのか」を説明できないと立場が危うくなる。
class ImpactsController < ApplicationController
  def index
    @kind = params[:kind].presence || "partner"
    @value = params[:value].presence
    @summary = ImpactQuery.new(
      organization_id: current_organization.id, kind: @kind, value: @value
    ).call

    # 起点の選択肢。実際に使われているものだけを出す。
    @thresholds = Threshold.where(organization_id: current_organization.id)
                           .order(id: :desc).limit(50)
    @aliases = api.learned_aliases
  rescue ApiClient::Unreachable
    @aliases = []
  end
end
