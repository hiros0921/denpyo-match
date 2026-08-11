class DocumentsController < ApplicationController
  def index
    @client_id = params[:client_id].presence
    scope = Document.joins(:client)
                    .where(clients: { organization_id: current_organization.id })
    scope = scope.where(client_id: @client_id) if @client_id
    @documents = scope.includes(:client, :match_result, :jobs)
                      .order(id: :desc).limit(100)
    # 一覧の上に出す集計。現場が最初に見るのは「今どれだけ溜まっているか」。
    @counts = scope.group(:status).count
  end

  def new
    @clients = accessible_clients
  end

  # アップロードは Go の API へ中継する。
  #
  # Rails が直接 documents と jobs を作らないのは、受付の手順を1箇所に保つため。
  # 2箇所で作ると、片方だけ直したときに片方の経路だけ壊れる。
  # 実際、伝票とジョブを別々に作ると「ジョブの無い伝票」が生まれ、
  # 永久に処理されないまま画面に「受付」で残る。
  def create
    file = params[:file]
    if file.blank?
      redirect_to new_document_path, alert: "ファイルを選んでください" and return
    end

    res = ApiClient.new.upload(
      file: file,
      client_id: params[:client_id],
      doc_type: params[:doc_type].presence || 1,
      direction: params[:direction].presence
    )
    if res[:ok]
      # 1枚の紙に複数の伝票があれば、受付で分かれている。
      # 何件になったかを言わないと、10枚入れたのに1件だと思われる。
      note = if res[:count].to_i > 1
               "#{res[:count]}件の伝票として受け付けました。1枚に複数あったので分けています"
             else
               "受け付けました。処理が終わるまでこの画面で確認できます"
             end
      redirect_to document_path(res[:document_id]), notice: note
    else
      redirect_to new_document_path, alert: res[:error]
    end
  rescue ApiClient::Unreachable => e
    redirect_to new_document_path,
                alert: "処理サーバーに繋がりません（#{e.message}）"
  end

  def show
    @document = find_document!(params[:id])
    @job = @document.latest_job
    @result = @document.match_result
    @fields = @document.extracted_fields.order(:field_key)
    # 2位以下が見えないと人は判断できない。1位だけ出す画面にしない。
    @candidates = @document.match_candidates
                           .includes(:partner).order(:rank).limit(10)
    # インボイス登録番号の検査結果。受領側でしか意味を持たないが、
    # 発行側でも自社の番号が正しく入っているかの確認に使える。
    @reg = ActiveRecord::Base.connection.exec_query(
      "SELECT reg_no, status, why FROM invoice_reg_checks WHERE document_id = $1",
      "RegCheck", [@document.id]
    ).first
  end

  def decide
    @document = find_document!(params[:id])
    res = ApiClient.new.decide(
      document_id: @document.id,
      organization_id: current_organization.id,
      actor_id: current_user.id,
      decision: params[:decision].to_i,
      partner_id: params[:partner_id].presence,
      learn_alias: params[:learn_alias].presence
    )
    if res[:ok]
      redirect_back fallback_location: reviews_path, notice: "記録しました"
    else
      redirect_back fallback_location: reviews_path, alert: res[:error]
    end
  rescue ApiClient::Unreachable => e
    redirect_back fallback_location: reviews_path,
                  alert: "処理サーバーに繋がりません（#{e.message}）"
  end
end
