# 入出金との突合。
#
# 受領した伝票に対して「本当にその支払いがあったか」を確かめる画面。
# 取引先マスタとの照合が「誰からの請求か」を決めるのに対し、
# こちらは「実際にお金が動いたか」を決める。会計事務所が
# 記帳の根拠にできるのは後者。
class SettlementsController < ApplicationController
  def index
    @client_id = params[:client_id].presence
    @only = params[:only].presence || "attention"
    q = SettlementQuery.new(current_organization.id,
                            client_id: @client_id, only: @only)
    @counts = q.counts
    @unsettled = q.unsettled_count
    @total = q.total
    @rows = q.rows
    @batches = q.batches
    @clients = accessible_clients
  end

  # 明細CSVの取り込み。Go の API へ中継する。
  #
  # Rails が直接 transactions を作らないのは、正規化を C++ で行うため。
  # 経路を2つにすると、片方だけ正規化を通す事故が起きる。
  def import
    file = params[:file]
    if file.blank?
      redirect_to settlements_path, alert: "ファイルを選んでください" and return
    end
    res = api.import_transactions(
      file: file,
      client_id: params[:client_id],
      source_type: params[:source_type]
    )
    if res[:ok]
      s = res[:settle]
      note = "#{res[:rows]}件を取り込みました"
      note += "（#{res[:skipped]}件は取り込み済みのため飛ばしました）" if res[:skipped].to_i > 0
      if s
        note += "。突合: 自動#{s['auto']} / 要確認#{s['review']} / 相手なし#{s['none']}"
      end
      redirect_to settlements_path(client_id: params[:client_id]), notice: note
    else
      redirect_to settlements_path, alert: res[:error]
    end
  rescue ApiClient::Unreachable => e
    redirect_to settlements_path, alert: "処理サーバーに繋がりません（#{e.message}）"
  end

  # 突合だけを再実行する。別名を覚えた後や、閾値を変えた後に使う。
  def run
    res = api.run_settlements(client_id: params[:client_id])
    if res[:ok]
      s = res[:stats]
      redirect_to settlements_path(client_id: params[:client_id]),
                  notice: "突合しました。自動#{s['auto']} / 要確認#{s['review']} / " \
                          "相手なし#{s['none']} / 変更なし#{s['kept']}"
    else
      redirect_to settlements_path, alert: res[:error]
    end
  rescue ApiClient::Unreachable => e
    redirect_to settlements_path, alert: "処理サーバーに繋がりません（#{e.message}）"
  end

  # 人の確定。この伝票は以後、自動突合が上書きしない。
  def confirm
    document = find_document!(params[:id])
    res = api.confirm_settlement(
      document_id: document.id,
      actor_id: current_user.id,
      transaction_id: params[:transaction_id].presence,
      none: params[:none].present?,
      learn_alias: params[:learn_alias].presence
    )
    if res[:ok]
      note = "記録しました"
      note += "（#{res[:alias_error]}）" if res[:alias_error]
      redirect_back fallback_location: settlements_path, notice: note
    else
      redirect_back fallback_location: settlements_path, alert: res[:error]
    end
  rescue ApiClient::Unreachable => e
    redirect_back fallback_location: settlements_path,
                  alert: "処理サーバーに繋がりません（#{e.message}）"
  end
end
