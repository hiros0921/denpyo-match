class ApplicationController < ActionController::Base
  before_action :authenticate_user!

  # 組織をまたいで見えないようにする。
  # 会計事務所は複数の顧問先の帳簿を預かっているので、
  # 「別の事務所のデータが見える」は起きた時点で契約が終わる。
  # 各画面で書き忘れないよう、絞り込みの起点をここに1つだけ置く。
  helper_method :current_organization, :accessible_clients, :billing_status

  private

  # 契約の状態。1リクエストにつき1回だけ取る。
  #
  # 猶予期間に入っていることは、どの画面にいても見えたほうがよい。
  # 契約の画面を開いたときだけ分かる作りだと、気付くのが遅れて
  # 14日を使い切る。全画面の帯に出すために、ここで取る。
  #
  # 繋がらないときは nil。決済サーバーの不調で画面が開けなくなるのは、
  # 「取り返しがつかないのは止めたほう」という方針に反する。
  def billing_status
    return @billing_status if defined?(@billing_status)
    @billing_status = ApiClient.new.billing_status(
      organization_id: current_organization.id
    )
  rescue ApiClient::Unreachable
    @billing_status = nil
  end

  def current_organization
    @current_organization ||= current_user.organization
  end

  def accessible_clients
    @accessible_clients ||= current_organization.clients.order(:id)
  end

  # 伝票は必ずこの経路で引く。Document.find を直接呼ばない。
  def find_document!(id)
    Document.joins(:client)
            .where(clients: { organization_id: current_organization.id })
            .find(id)
  end
end
