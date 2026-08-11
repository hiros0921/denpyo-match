# 覚えた表記。
#
# 承認画面で候補を押すと、その伝票に書かれていた表記が別名として貯まる。
# これは「使うほど当たるようになる」仕組みの中心で、実測でも効果が出た。
#   誤読された表記を人が1件教えた結果、却下5件→4件、自動承認12件→13件。誤承認はゼロのまま。
#
# ただし同じ強さで逆にも効く。誤って覚えると、その取引先はずっと当たらなくなる。
# 押し間違いは現場で必ず起きるので、見て取り消せるようにしておく。
class LearnedAliasesController < ApplicationController
  def index
    @aliases = ApiClient.new.learned_aliases(organization_id: current_organization.id)
  rescue ApiClient::Unreachable => e
    @aliases = []
    flash.now[:alert] = "処理サーバーに繋がりません（#{e.message}）"
  end

  def destroy
    res = ApiClient.new.forget_alias(
      id: params[:id], organization_id: current_organization.id,
      actor_id: current_user.id
    )
    if res[:ok]
      # 「無かったことにする」のではなく「取り消した」を記録している。
      # partner_aliases の行は消えるが、覚えた事実と取り消した事実は監査ログに残る。
      redirect_to learned_aliases_path,
                  notice: "取り消しました（監査ログには記録が残ります）"
    else
      redirect_to learned_aliases_path, alert: res[:error]
    end
  rescue ApiClient::Unreachable => e
    redirect_to learned_aliases_path, alert: "処理サーバーに繋がりません（#{e.message}）"
  end
end
